package server

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"context"
)

func (d *Daemon) startVoiceSupervisor(ctx context.Context) {
	vc := d.cfg.Voice
	if !vc.Manage {
		return
	}
	if vc.AppID == "" {
		d.logger.Warn("voice: manage enabled but voice.app_id is empty")
		return
	}
	binary := vc.Binary
	if binary == "" {
		if exe, err := os.Executable(); err == nil {
			binary = filepath.Join(filepath.Dir(exe), "digitorn-voice")
		}
	}
	if binary == "" {
		d.logger.Warn("voice: manage enabled but no binary resolved")
		return
	}
	if _, err := os.Stat(binary); err != nil {
		d.logger.Warn("voice: managed binary not found",
			slog.String("path", binary), slog.String("err", err.Error()))
		return
	}

	httpAddr := vc.HTTPAddr
	if httpAddr == "" {
		httpAddr = ":9091"
	}
	env := append(os.Environ(),
		fmt.Sprintf("DIGITORN_VOICE_BASE_URL=http://127.0.0.1:%d", d.cfg.Server.Port),
		"DIGITORN_VOICE_APP_ID="+vc.AppID,
		"DIGITORN_VOICE_HTTP_ADDR="+httpAddr,
	)
	if vc.Transport != "" {
		env = append(env, "DIGITORN_VOICE_TRANSPORT="+vc.Transport)
	}
	if vc.Engine != "" {
		env = append(env, "DIGITORN_VOICE_ENGINE="+vc.Engine)
	}
	if vc.Asterisk != "" {
		env = append(env, "DIGITORN_VOICE_ASTERISK_ADDR="+vc.Asterisk)
	}
	if vc.Twilio != "" {
		env = append(env, "DIGITORN_VOICE_TWILIO_ADDR="+vc.Twilio)
	}
	if vc.STTModel != "" {
		env = append(env, "DIGITORN_VOICE_STT_MODEL="+vc.STTModel)
	}
	if vc.TTSModel != "" {
		env = append(env, "DIGITORN_VOICE_TTS_MODEL="+vc.TTSModel)
	}
	if vc.TTSVoice != "" {
		env = append(env, "DIGITORN_VOICE_TTS_VOICE="+vc.TTSVoice)
	}
	if vc.Language != "" {
		env = append(env, "DIGITORN_VOICE_LANGUAGE="+vc.Language)
	}

	healthURL := "http://127.0.0.1" + httpAddr + "/healthz"
	if !strings.HasPrefix(httpAddr, ":") {
		healthURL = "http://" + httpAddr + "/healthz"
	}
	go d.superviseBackground(ctx, binary, env, healthURL)
}
