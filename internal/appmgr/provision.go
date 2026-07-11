package appmgr

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// hubProvisionPublisher is the hub publisher that hosts Digitorn's server-channel
// apps (excalidraw, latex, drawdb, univer). Their bundles (web/dist included) live
// on the hub, out of the daemon binary; the daemon fetches them post-boot.
const hubProvisionPublisher = "digitorn"

// ReconcileHubApps installs/updates every server-channel app whose source is the
// hub (provision: hub in channels.yaml). For each: if it's missing, or installed
// at a version older than the hub's latest, it is (re)installed from
// hub://digitorn/<app>. Per-app failures are logged and skipped — one bad app
// never blocks the others, and the whole call is meant to run in a background
// goroutine AFTER boot so startup is never delayed by network/hub latency.
//
// The hub's package-detail + download endpoints are public, so no token is
// needed for the daemon's own provisioning.
func (m *gormManager) ReconcileHubApps(ctx context.Context) {
	apps := hubProvisionedApps()
	if len(apps) == 0 {
		return
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, name := range apps {
		if ctx.Err() != nil {
			return
		}
		latest, err := m.hubLatest(ctx, client, hubProvisionPublisher, name, "")
		if err != nil {
			m.cfg.Logger.Warn("appmgr: hub provision — resolve latest failed",
				slog.String("app", name), slog.String("err", err.Error()))
			continue
		}
		if cur, err := m.GetApp(ctx, name); err == nil && cur != nil && cur.Enabled && cur.Version == latest {
			continue // installed, enabled, current
		}
		source := "hub://" + hubProvisionPublisher + "/" + name
		if _, err := m.Install(ctx, source, ""); err != nil {
			m.cfg.Logger.Warn("appmgr: hub provision — install failed",
				slog.String("app", name), slog.String("source", source), slog.String("err", err.Error()))
			continue
		}
		m.cfg.Logger.Info("appmgr: provisioned server app from hub",
			slog.String("app", name), slog.String("version", latest))
	}
}
