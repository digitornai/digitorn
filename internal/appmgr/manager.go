package appmgr

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/persistence/models"
)

// App is the public metadata view of one installed app. Returned by
// List() and GetApp() for the REST API and the frontend marketplace UI.
type App struct {
	AppID       string `json:"app_id"`
	Name        string `json:"name"`
	ShortName   string `json:"short_name,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Author      string `json:"author,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Color       string `json:"color,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Mode is the runtime execution model (conversation | background |
	// one_shot | pipeline), derived from the compiled manifest. Background is
	// true when the app is trigger/channel-driven — the signal the web client
	// uses to open the ops dashboard instead of the chat surface. Both are
	// populated from the snapshot for enabled apps ; empty/false otherwise.
	Mode        string    `json:"mode,omitempty"`
	Background  bool      `json:"background"`
	Modes       []AppMode `json:"modes,omitempty"`
	DefaultMode string    `json:"default_mode,omitempty"`
	// BYOK ("bring your own key") routes this app's LLM traffic directly
	// to the provider using the brain-declared credential, bypassing the
	// digitorn LLM gateway. Default false : the daemon uses the gateway
	// (commodity for signed-in digitorn users). Operator toggles via
	// PUT /api/apps/{app_id}/byok.
	BYOK        bool      `json:"byok"`
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Activity is the compiled ``ui.activity`` block, surfaced on the
	// summary so the composer + workspace can gate the Activity pane
	// without fetching the full manifest.
	Activity *schema.ActivityPanelBlock `json:"activity,omitempty"`
}

// RuntimeApp is what the runtime consumes : the decoded app.dgc
// definition plus the path to the bundle dir on disk (needed when
// modules need to read prompts/, skills/, assets/ at execution time).
type RuntimeApp struct {
	Meta       *App
	Definition *schema.AppDefinition
	BundleDir  string // absolute path to {Root}/{app_id}/
}

// UpdateInfo answers "/check-update" for hub-installed apps : compares
// the local Version against the hub's latest_version.
type UpdateInfo struct {
	AppID           string `json:"app_id"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
}

// Manager is the public app-manager API. The runtime uses Get() ; REST
// handlers use everything else.
type Manager interface {
	// Install copies a source dir into the install root, compiles it,
	// writes app.dgc, and upserts the DB row. `source` is one of :
	//   /abs/or/relative/path    — local filesystem
	//   hub://publisher/pkg@1.0  — digitorn hub
	//   builtin://name           — built-in example bundle
	// The userJWT is forwarded to the hub as Bearer for hub:// sources
	// (empty for other sources).
	Install(ctx context.Context, source, userJWT string) (*App, error)

	// Upgrade is an alias of Install for an existing app — runs the
	// same flow with the new source and overwrites in-place.
	Upgrade(ctx context.Context, appID, source, userJWT string) (*App, error)

	// Uninstall removes the DB row and the install dir. If purge is
	// true, the caller is responsible for deleting any session state
	// of this app (we don't reach into the session store from here).
	Uninstall(ctx context.Context, appID string, purge bool) error

	// Enable / Disable flip the Enabled flag and update the snapshot.
	Enable(ctx context.Context, appID string) error
	Disable(ctx context.Context, appID string) error

	// SetBYOK toggles the app's BYOK routing flag and republishes the
	// snapshot so the next runtime turn picks up the change without
	// daemon restart. Returns ErrAppNotFound if the row is missing.
	SetBYOK(ctx context.Context, appID string, enabled bool) error

	SetAppPieces(ctx context.Context, appID string, pieces []string) error

	// SetDisplayName overrides the displayed label (trimmed; "" clears the
	// override → falls back to the bundle's short name). Survives reload.
	// Returns ErrAppNotFound if the row is missing.
	SetDisplayName(ctx context.Context, appID, name string) error

	// Reload recompiles the app from its on-disk source (used when the
	// operator edits app.yaml by hand) and refreshes the snapshot.
	Reload(ctx context.Context, appID string) error

	// CheckUpdate hits the hub for hub-installed apps.
	CheckUpdate(ctx context.Context, appID, userJWT string) (*UpdateInfo, error)

	// List returns every app in the DB. includeDisabled=false filters
	// to Enabled=true.
	List(ctx context.Context, includeDisabled bool) ([]App, error)

	// ListDisabled returns apps where Enabled=false.
	ListDisabled(ctx context.Context) ([]App, error)

	// GetApp returns one app's metadata.
	GetApp(ctx context.Context, appID string) (*App, error)

	// Get returns the runtime view of an enabled app. Hot path — reads
	// from the lock-free snapshot, no DB round-trip.
	Get(ctx context.Context, appID string) (*RuntimeApp, error)

	// GetManifest returns the compiled AppDefinition for an app.
	GetManifest(ctx context.Context, appID string) (*schema.AppDefinition, error)

	// Bootstrap loads every enabled app from the DB at daemon startup,
	// decoding their app.dgc and populating the snapshot. Apps whose
	// bundle is missing/corrupt are logged and skipped, never fatal.
	Bootstrap(ctx context.Context) error
}

// Config configures a Manager. The compiler is shared — the manager
// uses it for install/reload but never owns its lifecycle.
type Config struct {
	DB       *gorm.DB
	Root     string             // absolute install root, e.g. ~/.digitorn/apps
	Hub      HubConfig          // hub client settings
	Compiler *compiler.Compiler // compiler instance (must be configured with manifest sources)
	Logger   *slog.Logger
	Channel  string // "server" → route builtins via channels.yaml; "" → seed-if-missing
}

// HubConfig is what Manager needs from config.Apps.Hub.
type HubConfig struct {
	URL             string
	Timeout         time.Duration
	VerifySSL       bool
	MaxArchiveBytes int64
}

// gormManager is the GORM-backed implementation of Manager.
type gormManager struct {
	cfg Config

	// Per-app locks : two installs of the SAME app serialize ; two
	// installs of DIFFERENT apps run in parallel. Lazy-initialized.
	locks sync.Map // map[string]*sync.Mutex

	// Lock-free read snapshot of currently-enabled apps. Swapped
	// atomically on install / enable / disable / reload / uninstall.
	snap atomic.Pointer[snapshot]
}

// snapshot is an immutable map of app_id → RuntimeApp. Reads are
// lock-free via atomic.Pointer ; writes clone-and-swap.
type snapshot struct {
	apps map[string]*RuntimeApp
}

// New constructs a Manager. The DB schema must already be migrated by
// the caller (via db.AutoMigrate).
func New(cfg Config) (Manager, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("appmgr: nil DB")
	}
	if cfg.Root == "" {
		return nil, fmt.Errorf("appmgr: empty Root")
	}
	if cfg.Compiler == nil {
		return nil, fmt.Errorf("appmgr: nil Compiler")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m := &gormManager{cfg: cfg}
	m.snap.Store(&snapshot{apps: map[string]*RuntimeApp{}})
	return m, nil
}

// lockFor returns (or lazily creates) the per-app mutex.
func (m *gormManager) lockFor(appID string) *sync.Mutex {
	v, _ := m.locks.LoadOrStore(appID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// readSnapshot returns the current snapshot. Lock-free.
func (m *gormManager) readSnapshot() *snapshot {
	return m.snap.Load()
}

// swapSnapshot replaces one entry by app_id (or removes it if ra is
// nil) and atomically publishes the new snapshot. The previous map is
// never mutated — readers in flight see a consistent view.
func (m *gormManager) swapSnapshot(appID string, ra *RuntimeApp) {
	for {
		old := m.snap.Load()
		next := &snapshot{apps: make(map[string]*RuntimeApp, len(old.apps)+1)}
		for k, v := range old.apps {
			if k == appID {
				continue
			}
			next.apps[k] = v
		}
		if ra != nil {
			next.apps[appID] = ra
		}
		if m.snap.CompareAndSwap(old, next) {
			return
		}
	}
}

// metaFromRow converts a DB row to the public App type.
func metaFromRow(r *models.App) *App {
	shortName := r.ShortName
	if r.DisplayName != "" {
		shortName = r.DisplayName
	}
	return &App{
		AppID:       r.AppID,
		Name:        r.Name,
		ShortName:   shortName,
		Version:     r.Version,
		Description: r.Description,
		Category:    r.Category,
		Author:      r.Author,
		Icon:        r.Icon,
		Color:       r.Color,
		Enabled:     r.Enabled,
		BYOK:        r.BYOK,
		InstalledAt: r.InstalledAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// deriveMode reads the runtime execution model from a compiled definition.
// background is true for non-conversation modes OR when the app declares a
// channels module or runtime triggers (the channels-based apps leave
// runtime.mode empty, so mode alone is not a reliable discriminant).
type AppMode struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Accent      string `json:"accent,omitempty"`
}

func deriveModes(def *schema.AppDefinition) (modes []AppMode, defaultMode string) {
	if def == nil || def.Runtime == nil || len(def.Runtime.Modes) == 0 {
		return nil, ""
	}
	order := def.Runtime.ModesOrder
	if len(order) == 0 {
		order = make([]string, 0, len(def.Runtime.Modes))
		for id := range def.Runtime.Modes {
			order = append(order, id)
		}
	}
	modes = make([]AppMode, 0, len(order))
	for _, id := range order {
		md, ok := def.Runtime.Modes[id]
		if !ok {
			continue
		}
		modes = append(modes, AppMode{
			ID:          id,
			Label:       md.Label,
			Description: md.Description,
			Icon:        md.Icon,
			Accent:      md.Accent,
		})
	}
	if _, ok := def.Runtime.Modes["auto"]; ok {
		defaultMode = "auto"
	} else if len(modes) > 0 {
		defaultMode = modes[0].ID
	}
	return modes, defaultMode
}

func deriveMode(def *schema.AppDefinition) (mode string, background bool) {
	if def == nil {
		return "", false
	}
	mode = string(def.App.Mode)
	if def.Runtime != nil && def.Runtime.Mode != "" {
		mode = string(def.Runtime.Mode)
	}
	switch mode {
	case "background", "one_shot", "pipeline":
		background = true
	}
	if def.Tools != nil {
		if _, ok := def.Tools.Modules["channels"]; ok {
			background = true
		}
	}
	if def.Runtime != nil && len(def.Runtime.Triggers) > 0 {
		background = true
	}
	return mode, background
}

// enrichMeta fills Mode/Background on a public App from the live snapshot
// (enabled apps only). Disabled apps absent from the snapshot keep the zero
// values — the dashboard only opens enabled apps.
func (m *gormManager) enrichMeta(meta *App) {
	if meta == nil {
		return
	}
	if ra, ok := m.readSnapshot().apps[meta.AppID]; ok && ra != nil {
		meta.Mode, meta.Background = deriveMode(ra.Definition)
		meta.Modes, meta.DefaultMode = deriveModes(ra.Definition)
		if ra.Definition != nil && ra.Definition.UI != nil {
			meta.Activity = ra.Definition.UI.Activity
		}
	}
}
