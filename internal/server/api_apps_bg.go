package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/digitornai/digitorn/internal/appmgr"
	bgchannels "github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/service"
)

func (d *Daemon) pushTriggersToBackground(ctx context.Context, app *appmgr.App) {
	d.pushTriggersAs(ctx, app, "", "")
}

func (d *Daemon) purgeTriggersFromBackground(ctx context.Context, appID string) {
	if d.cfg.Background.OpsURL == "" || appID == "" {
		return
	}
	url := d.cfg.Background.OpsURL + "/ops/triggers?app=" + neturl.QueryEscape(appID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return
	}
	if d.cfg.Background.OpsToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.cfg.Background.OpsToken)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		d.logger.Warn("background: purge triggers failed", slog.String("app", appID), slog.String("err", err.Error()))
		return
	}
	resp.Body.Close()
	d.logger.Info("background: triggers purged", slog.String("app", appID), slog.Int("status", resp.StatusCode))
}

func (d *Daemon) pushTriggersAs(ctx context.Context, app *appmgr.App, owner, refreshToken string) {
	if d.cfg.Background.OpsURL == "" {
		return
	}
	def, err := d.appMgr.GetManifest(ctx, app.AppID)
	if err != nil || def == nil || def.Tools == nil {
		return
	}
	chMod, ok := def.Tools.Modules["channels"]
	if !ok {
		return
	}
	cfgRaw, ok := chMod.Config["providers"]
	if !ok {
		return
	}
	b, err := json.Marshal(map[string]any{"providers": cfgRaw})
	if err != nil {
		return
	}
	var chanCfg bgchannels.ModuleConfig
	if err := json.Unmarshal(b, &chanCfg); err != nil {
		return
	}
	chanCfg.Normalize()

	base := d.cfg.Background.OpsURL
	client := &http.Client{Timeout: 10 * time.Second}

	for name, pc := range chanCfg.Providers {
		if !pc.IsEnabled() {
			continue
		}
		act := pc.Activation
		if act.Session == "" {
			act.Session = bgchannels.SessionPerEvent
		}
		if act.Reply == "" {
			act.Reply = bgchannels.ReplyNone
		}
		resolvedConfig, _ := d.resolveConfigSecrets(app.AppID, pc.Config).(map[string]any)
		if resolvedConfig == nil {
			resolvedConfig = pc.Config
		}
		req := service.CreateTriggerRequest{
			AppID:        app.AppID,
			Provider:     name,
			Adapter:      pc.Adapter,
			Config:       resolvedConfig,
			Activation:   &act,
			Owner:        owner,
			RefreshToken: refreshToken,
		}
		body, err := json.Marshal(req)
		if err != nil {
			continue
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/ops/triggers", bytes.NewReader(body))
		if err != nil {
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if d.cfg.Background.OpsToken != "" {
			httpReq.Header.Set("Authorization", "Bearer "+d.cfg.Background.OpsToken)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			d.logger.Warn("background: push trigger failed", slog.String("app", app.AppID), slog.String("provider", name), slog.String("err", err.Error()))
			continue
		}
		resp.Body.Close()
		d.logger.Info("background: trigger pushed", slog.String("app", app.AppID), slog.String("provider", name), slog.String("adapter", pc.Adapter))
	}
}
