package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (d *Daemon) startBackgroundSupervisor(ctx context.Context) {
	bg := d.cfg.Background
	if !bg.Manage {
		return
	}
	binary := bg.Binary
	if binary == "" {
		if exe, err := os.Executable(); err == nil {
			binary = filepath.Join(filepath.Dir(exe), "digitorn-background")
		}
	}
	if binary == "" {
		d.logger.Warn("background: manage enabled but no binary resolved")
		return
	}
	if _, err := os.Stat(binary); err != nil {
		d.logger.Warn("background: managed binary not found",
			slog.String("path", binary), slog.String("err", err.Error()))
		return
	}

	env := append(os.Environ(),
		"DIGITORN_BG_HTTP_ADDR="+hostPort(bg.OpsURL, "127.0.0.1:8090"),
		fmt.Sprintf("DIGITORN_BG_DAEMON_URL=http://127.0.0.1:%d", d.cfg.Server.Port),
		"DIGITORN_BG_APPS_DIR="+d.cfg.Apps.Root,
	)
	if os.Getenv("DIGITORN_BG_DB_DSN") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dbPath := filepath.Join(home, ".digitorn", "digitorn-background.db")
			_ = os.MkdirAll(filepath.Dir(dbPath), 0o755)
			env = append(env, "DIGITORN_BG_DB_DSN="+dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
		}
	}
	if bg.ServiceJWT != "" {
		env = append(env, "DIGITORN_BG_SERVICE_JWT="+bg.ServiceJWT)
	}
	if bg.OpsToken != "" {
		env = append(env, "DIGITORN_BG_OPS_TOKEN="+bg.OpsToken)
	}
	healthURL := strings.TrimRight(bg.OpsURL, "/") + "/healthz"

	go d.superviseBackground(ctx, binary, env, healthURL)
}

func (d *Daemon) superviseBackground(ctx context.Context, binary string, env []string, healthURL string) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, binary)
		cmd.Env = env
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr

		start := time.Now()
		if err := cmd.Start(); err != nil {
			d.logger.Error("background: spawn failed", slog.String("err", err.Error()))
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		d.logger.Info("background: service started (managed)", slog.Int("pid", cmd.Process.Pid))

		hctx, hcancel := context.WithCancel(ctx)
		go d.watchBackgroundHealth(hctx, cmd, healthURL)
		waitErr := cmd.Wait()
		hcancel()

		if ctx.Err() != nil {
			return
		}
		d.logger.Warn("background: service exited, restarting",
			slog.String("err", errString(waitErr)), slog.Duration("uptime", time.Since(start)))
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func (d *Daemon) watchBackgroundHealth(ctx context.Context, cmd *exec.Cmd, healthURL string) {
	if !strings.HasSuffix(healthURL, "/healthz") {
		return
	}
	if !sleepCtx(ctx, 15*time.Second) {
		return
	}
	client := &http.Client{Timeout: 4 * time.Second}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := client.Get(healthURL)
			ok := err == nil && resp.StatusCode == http.StatusOK
			if resp != nil {
				resp.Body.Close()
			}
			if ok {
				fails = 0
				continue
			}
			fails++
			if fails >= 3 {
				d.logger.Warn("background: unhealthy, killing to trigger restart", slog.Int("fails", fails))
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				return
			}
		}
	}
}

func hostPort(rawURL, fallback string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fallback
	}
	return u.Host
}

func nextBackoff(cur, max time.Duration) time.Duration {
	n := cur * 2
	if n > max {
		return max
	}
	return n
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func errString(err error) string {
	if err == nil {
		return "clean exit"
	}
	return err.Error()
}
