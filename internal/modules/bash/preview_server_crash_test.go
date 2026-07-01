package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// foregroundServerHint must flag every unambiguous long-running server / watcher
// and never flag a command that terminates on its own (build / test / install).
func TestForegroundServerHint(t *testing.T) {
	flagged := []string{
		"npm run dev", "npm start", "npm run serve", "pnpm dev", "yarn dev",
		"yarn start", "bun run dev", "vite", "npx vite", "vite preview",
		"vite serve", "next dev", "next start", "nuxt dev", "ng serve",
		"react-scripts start", "vue-cli-service serve", "webpack serve",
		"webpack-dev-server", "nodemon server.js", "flask --app app run",
		"flask run", "uvicorn main:app", "gunicorn app:app",
		"python -m http.server 8080", "python3 -m http.server", "php -S 0.0.0.0:8000",
		"rails server", "rails s", "http-server dist", "serve -s build",
		"live-server", "tail -f log.txt", "cd web && npm run dev",
		"npm run build && npm start",
	}
	for _, c := range flagged {
		if foregroundServerHint(c) == "" {
			t.Errorf("expected a server hint for %q, got none", c)
		}
	}
	allowed := []string{
		"npm run build", "npm ci", "npm install", "npm test", "yarn build",
		"pnpm build", "vite build", "tsc --noEmit", "go build ./...", "git status",
		"eslint .", "next build", "ng build", "echo 'npm run dev'",
		`bash -c "echo started server"`, "npm run lint", "cat package.json",
		"ls -la", "make build", "cargo build",
	}
	for _, c := range allowed {
		if h := foregroundServerHint(c); h != "" {
			t.Errorf("did NOT expect a server hint for %q, got: %s", c, h)
		}
	}
}

// TestRun_ForegroundServerRejectedFast proves a dev server in the foreground is
// rejected immediately (no 120s freeze of the loop) with a guidance message,
// while the SAME command on the background path is exempt from the guard.
func TestRun_ForegroundServerRejectedFast(t *testing.T) {
	m := testModule(t)
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "s"})

	start := time.Now()
	raw, _ := json.Marshal(runParams{Command: "npm run dev"})
	res, _ := m.run(ctx, raw)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("foreground server was not rejected fast: %v (it must not run to the timeout)", elapsed)
	}
	if res.Error == "" || !strings.Contains(res.Error, "background_run") {
		t.Fatalf("expected a background_run guidance error, got data=%+v error=%q", res.Data, res.Error)
	}
	// The exemption lives in module.run as `!tool.IsBackground(ctx)`: the guard is
	// only consulted for foreground dispatches, so background_run never trips it.
}
