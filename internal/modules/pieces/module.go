// Package pieces bridges Activepieces connectors into Digitorn as native tools.
// The bridge subprocess (digitorn-ap-bridge, built with Bun) speaks MCP stdio;
// this module wraps it as a LiveTooler so all installed piece actions appear in
// the agent's tool roster exactly like bash, database, or any other module.
//
// Auth lifecycle: per-user credentials are sealed in the InstalledPiece table.
// On each Invoke() the module unseals the caller's credentials and injects
// _ap_auth into the bridge call. The bridge never stores credentials.
//
// Trigger lifecycle: the bridge's trigger HTTP server (default :9234) handles
// polling and webhook callbacks. The background adapter calls it via HTTP.
package pieces

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"io"
	"net/http"
	"path/filepath"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/server/mcpoauth"
	"github.com/digitornai/digitorn/pkg/module"
	"gorm.io/gorm"
)

// globalDB and globalSealer are set by Setup() during daemon bootstrap, before
// the module registry instantiates modules. In-process only.
var (
	globalDB     *gorm.DB
	globalSealer *mcpoauth.Sealer
	setupMu      sync.Mutex
)

// Setup wires daemon-level resources into the pieces module. Must be called
// before daemon starts modules (i.e. before bootstrap calls StartExcept).
func Setup(db *gorm.DB, sealer *mcpoauth.Sealer) {
	setupMu.Lock()
	defer setupMu.Unlock()
	globalDB = db
	globalSealer = sealer
}

// Module is the in-process Digitorn module for Activepieces connectors.
type Module struct {
	module.Base
	bridge      *Bridge
	store       *Store
	piecesDir   string
	mu          sync.RWMutex
	reloadMu    sync.Mutex
	reloadTimer *time.Timer
}

func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:      "pieces",
		Version: "1.0.0",
		Description: "715+ Activepieces connectors as native agent tools. " +
			"Install pieces from the hub and their actions appear here automatically.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
	}
	setupMu.Lock()
	db := globalDB
	sealer := globalSealer
	setupMu.Unlock()

	if db != nil {
		m.store = newStore(db, sealer)
	}

	bridgePath := os.Getenv("DIGITORN_AP_BRIDGE_PATH")
	if bridgePath == "" {
		bridgePath = defaultBridgePath()
	}
	piecesDir := os.Getenv("DIGITORN_PIECES_DIR")
	if piecesDir == "" {
		piecesDir = defaultPiecesDir()
	}

	m.piecesDir = piecesDir
	m.bridge = newBridge(bridgePath, piecesDir, defaultTriggerPort, nil)
	return m
}

func (m *Module) Start(ctx context.Context) error {
	if err := m.Base.Start(ctx); err != nil {
		return err
	}
	// Bridge start is best-effort: if the binary isn't deployed yet, the module
	// starts successfully but returns zero tools until the bridge is present.
	if err := m.bridge.Start(ctx); err != nil {
		// Log but don't fail daemon boot.
		_, _ = fmt.Fprintf(os.Stderr, "pieces: bridge start deferred (%v)\n", err)
	}
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	m.bridge.Stop(ctx)
	return m.Base.Stop(ctx)
}

// LiveTools returns all installed piece actions as native tool.Specs.
// Implements domainmodule.LiveTooler.
func (m *Module) LiveTools(ctx context.Context) []tool.Spec {
	specs, _ := m.bridge.ListTools(ctx)
	return specs
}

// Invoke routes a piece tool call to the bridge with auth injected.
func (m *Module) Invoke(ctx context.Context, toolName string, params []byte) (tool.Result, error) {
	piece, _, ok := parseAPTool(toolName)
	if !ok {
		return tool.Result{Success: false, Error: "pieces: unroutable tool " + toolName},
			fmt.Errorf("pieces: unroutable tool %q", toolName)
	}

	augmented, err := m.injectAuth(ctx, piece, params)
	if err != nil {
		// Auth not found: tool is usable but will fail at the connector level.
		augmented = params
	}

	res, callErr := m.bridge.CallTool(ctx, toolName, augmented)

	// Reactive self-healing: if the connector rejected the call for an expired
	// or invalid token, force a token refresh and retry once. This covers the
	// cases the proactive refresh misses (missing/wrong stored expiry, clock
	// skew) so the user never has to manually reconnect while the refresh token
	// is still valid.
	if isAuthFailure(res) && m.store != nil {
		if id, ok := tool.IdentityFromContext(ctx); ok && id.UserID != "" {
			if wire, refreshed := m.store.ForceRefresh(ctx, id.UserID, piece); refreshed {
				retried := reinjectAuth(augmented, wire)
				res, callErr = m.bridge.CallTool(ctx, toolName, retried)
			}
		}
	}

	return res, callErr
}

// isAuthFailure reports whether a bridge result looks like a provider auth
// rejection (expired/invalid token) that a token refresh could fix.
func isAuthFailure(res tool.Result) bool {
	if res.Success {
		return false
	}
	e := strings.ToLower(res.Error)
	if e == "" {
		return false
	}
	for _, s := range []string{
		"\"status\":401", "status 401", " 401", "unauthorized", "unauthenticated",
		"invalidauthenticationtoken", "invalid_grant", "invalid_token", "invalid token",
		"token expired", "expired token", "access token", "token has expired",
	} {
		if strings.Contains(e, s) {
			return true
		}
	}
	return false
}

// reinjectAuth replaces the _ap_auth field of an already-augmented params blob
// with a freshly refreshed auth wire, preserving all other fields.
func reinjectAuth(augmented json.RawMessage, wire *APAuthWire) json.RawMessage {
	var args map[string]any
	if len(augmented) > 0 {
		_ = json.Unmarshal(augmented, &args)
	}
	if args == nil {
		args = map[string]any{}
	}
	args["_ap_auth"] = wire
	raw, err := json.Marshal(args)
	if err != nil {
		return augmented
	}
	return raw
}

// injectAuth looks up the caller's credentials and merges _ap_auth + _ap_session
// into the params JSON.
func (m *Module) injectAuth(ctx context.Context, pieceName string, params []byte) (json.RawMessage, error) {
	identity, ok := tool.IdentityFromContext(ctx)
	if !ok || identity.UserID == "" {
		return params, fmt.Errorf("no user identity in context")
	}

	// Check per-app static auth first (if store unavailable or piece not installed
	// per-user, fall back to app config).
	var args map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &args)
	}
	if args == nil {
		args = map[string]any{}
	}

	// Session ID scopes the bridge's in-memory KV store.
	if args["_ap_session"] == nil && identity.SessionID != "" {
		args["_ap_session"] = identity.UserID + ":" + identity.SessionID
	}

	// Per-user credential from store.
	if m.store != nil {
		wire, err := m.store.RevealAuth(ctx, identity.UserID, pieceName)
		if err == nil && wire != nil {
			args["_ap_auth"] = wire
		}
	}

	// Fall back to per-app static auth declared in module config.
	if args["_ap_auth"] == nil {
		if cfg := appAuthFor(ctx, pieceName); cfg != nil {
			args["_ap_auth"] = cfg
		}
	}

	raw, err := json.Marshal(args)
	if err != nil {
		return params, err
	}
	return raw, nil
}

// appAuthFor reads static per-piece auth from the app's module config block.
func appAuthFor(ctx context.Context, pieceName string) *APAuthWire {
	raw := module.ModuleConfigFrom(ctx)
	if len(raw) == 0 {
		return nil
	}
	var cfg Config
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	ac, ok := cfg.StaticAuth[pieceName]
	if !ok {
		return nil
	}
	return &APAuthWire{
		Type:         ac.Type,
		Value:        ac.Value,
		Fields:       ac.Fields,
		AccessToken:  ac.AccessToken,
		RefreshToken: ac.RefreshToken,
		TokenType:    ac.TokenType,
		Username:     ac.Username,
		Password:     ac.Password,
	}
}

// ReloadBridge restarts the bridge subprocess (called after a piece is
// installed/uninstalled to refresh the tool list).
func (m *Module) ReloadBridge(ctx context.Context) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.reloadTimer != nil {
		m.reloadTimer.Stop()
	}
	m.reloadTimer = time.AfterFunc(2*time.Second, func() {
		_ = m.bridge.Restart(context.Background())
	})
	return nil
}

// PiecesStore returns the installed pieces credential store (may be nil if the
// module was started without DB access).
func (m *Module) PiecesStore() *Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store
}

// PiecesDir returns the directory where piece bundles are stored.
func (m *Module) PiecesDir() string {
	return m.piecesDir
}

// Bridge returns the bridge subprocess wrapper.
func (m *Module) Bridge() *Bridge {
	return m.bridge
}

// DownloadBundle fetches a piece bundle from bundleURL and writes it to
// <piecesDir>/<pieceName>.js. Creates the directory if needed.
func (m *Module) DownloadBundle(ctx context.Context, bundleURL, pieceName string) error {
	if err := os.MkdirAll(m.piecesDir, 0o755); err != nil {
		return fmt.Errorf("pieces: mkdir %q: %w", m.piecesDir, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bundleURL, nil)
	if err != nil {
		return fmt.Errorf("pieces: bundle request: %w", err)
	}
	req.Header.Set("User-Agent", "digitorn-daemon/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pieces: bundle download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pieces: bundle download HTTP %d from %s", resp.StatusCode, bundleURL)
	}

	dst := filepath.Join(m.piecesDir, pieceName+".js")
	f, err := os.CreateTemp(m.piecesDir, ".piece-download-*")
	if err != nil {
		return fmt.Errorf("pieces: temp file: %w", err)
	}
	tmpPath := f.Name()
	defer func() {
		f.Close()
		os.Remove(tmpPath) // no-op if rename succeeded
	}()

	if _, err := io.Copy(f, io.LimitReader(resp.Body, 32<<20)); err != nil {
		return fmt.Errorf("pieces: write bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("pieces: close bundle: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("pieces: install bundle: %w", err)
	}
	return nil
}
