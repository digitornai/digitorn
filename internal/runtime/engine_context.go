package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/context/ctxinject"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// contextSectionsText renders the app + agent YAML-declared context sections for
// THIS turn (user / session / date / custom data). Returns "" when nothing is
// declared or renders. Built per-turn (never cached) so one user's data is never
// baked into another's prompt. Covers sub-agents too (they carry the same UserJWT).
func (e *Engine) contextSectionsText(in TurnInput, agent *schema.Agent, app *appmgr.RuntimeApp, snap sessionstore.SessionSnapshot) string {
	if app == nil || app.Definition == nil {
		return ""
	}
	var agentCtx *schema.ContextBlock
	if agent != nil {
		agentCtx = agent.Context
	}
	sections := ctxinject.Merge(app.Definition.Context, agentCtx)
	if len(sections) == 0 {
		return ""
	}
	data := ctxinject.Data{
		User: userBag(in.UserID, in.UserJWT),
		App: map[string]any{
			"id": app.Definition.App.AppID, "name": app.Definition.App.Name, "version": app.Definition.App.Version,
		},
		Session: sessionBag(snap, maxTokens(app)),
		Env: envBag(),
		Now:     time.Now(),
	}
	if agent != nil {
		data.Agent = map[string]any{"id": agent.ID, "role": agent.Role}
	}
	return ctxinject.Render(sections, data)
}

func envBag() map[string]any {
	home, _ := os.UserHomeDir()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = home + "/.config"
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		if goruntime.GOOS == "windows" {
			shell = "powershell"
		} else {
			shell = "bash"
		}
	}
	return map[string]any{
		"os":          goruntime.GOOS,
		"arch":        goruntime.GOARCH,
		"platform":    goruntime.GOOS,
		"shell":       shell,
		"home":        home,
		"config_home": configHome,
		"tmp":         os.TempDir(),
	}
}

func maxTokens(app *appmgr.RuntimeApp) int {
	if app == nil || app.Definition == nil || app.Definition.Runtime == nil || app.Definition.Runtime.Context == nil {
		return 0
	}
	return app.Definition.Runtime.Context.MaxTokens
}

func sessionBag(snap sessionstore.SessionSnapshot, ctxMaxTokens int) map[string]any {
	m := map[string]any{}
	if snap.SessionID != "" {
		m["id"] = snap.SessionID
	}
	if snap.Goal != "" {
		m["goal"] = snap.Goal
	}
	if snap.Title != "" {
		m["title"] = snap.Title
	}
	if snap.ActiveMode != "" {
		m["mode"] = snap.ActiveMode
	}
	if snap.TurnCount > 0 {
		m["turn"] = strconv.Itoa(snap.TurnCount)
		m["turns"] = strconv.Itoa(snap.TurnCount)
	}
	if snap.Workdir != "" {
		m["workdir"] = snap.Workdir
		m["workdir_slug"] = strings.NewReplacer(
			string(os.PathSeparator), "-",
			" ", "-",
		).Replace(snap.Workdir)
	}
	if snap.ContextTokens > 0 {
		m["tokens"] = strconv.Itoa(snap.ContextTokens)
		m["tokens_system"] = strconv.Itoa(snap.ContextSystemTokens)
		m["tokens_messages"] = strconv.Itoa(snap.ContextMessageTokens)
		m["tokens_tools"] = strconv.Itoa(snap.ContextToolsTokens)
		if ctxMaxTokens > 0 {
			pct := snap.ContextTokens * 100 / ctxMaxTokens
			m["context_pct"] = strconv.Itoa(pct)
			m["context_max"] = strconv.Itoa(ctxMaxTokens)
		}
	}
	if snap.TokensIn > 0 {
		m["tokens_in"] = strconv.FormatInt(snap.TokensIn, 10)
	}
	if snap.TokensOut > 0 {
		m["tokens_out"] = strconv.FormatInt(snap.TokensOut, 10)
	}
	if snap.UsdTotal > 0 {
		m["cost_usd"] = fmt.Sprintf("%.4f", snap.UsdTotal)
	}
	if n := len(snap.Facts); n > 0 {
		m["facts_count"] = strconv.Itoa(n)
	}
	if n := len(snap.Todos); n > 0 {
		m["todos_count"] = strconv.Itoa(n)
	}
	if snap.EventCount > 0 {
		m["event_count"] = strconv.FormatUint(snap.EventCount, 10)
	}
	return m
}

// userBag exposes every JWT claim (name, email, region, locale, roles, custom…)
// under user.* so a template can read user.<anything> the token carries. The token
// was already VERIFIED at the API edge — here we only DECODE the payload to read
// its claims (no re-verification).
func userBag(userID, jwt string) map[string]any {
	u := decodeJWTClaims(jwt)
	if u == nil {
		u = map[string]any{}
	}
	if userID != "" {
		u["id"] = userID
	}
	return u
}

// decodeJWTClaims base64url-decodes a JWT's payload segment into a claims map.
func decodeJWTClaims(token string) map[string]any {
	if token == "" {
		return nil
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}
