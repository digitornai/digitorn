package appmgr

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mbathepaul/digitorn/internal/compiler/codegen"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// List returns all apps in the DB, sorted by Name then AppID. If
// includeDisabled is false, only Enabled=true rows are returned.
func (m *gormManager) List(ctx context.Context, includeDisabled bool) ([]App, error) {
	q := m.cfg.DB.WithContext(ctx).Order("name ASC, app_id ASC")
	if !includeDisabled {
		q = q.Where("enabled = ?", true)
	}
	var rows []models.App
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]App, len(rows))
	for i := range rows {
		out[i] = *metaFromRow(&rows[i])
		m.enrichMeta(&out[i])
	}
	return out, nil
}

// ListDisabled is the shortcut for List(ctx, true) filtered to disabled.
func (m *gormManager) ListDisabled(ctx context.Context) ([]App, error) {
	var rows []models.App
	err := m.cfg.DB.WithContext(ctx).Where("enabled = ?", false).
		Order("name ASC, app_id ASC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]App, len(rows))
	for i := range rows {
		out[i] = *metaFromRow(&rows[i])
	}
	return out, nil
}

// GetApp returns one app's metadata by id. ErrAppNotFound if absent.
func (m *gormManager) GetApp(ctx context.Context, appID string) (*App, error) {
	var row models.App
	if err := m.cfg.DB.WithContext(ctx).First(&row, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrAppNotFound
		}
		return nil, err
	}
	meta := metaFromRow(&row)
	m.enrichMeta(meta)
	return meta, nil
}

// Get returns the runtime view of an enabled app. Hot path : reads the
// lock-free atomic snapshot. Returns ErrAppNotFound if the app is not
// in the snapshot (either absent or disabled). Falls back to a DB
// lookup with disk load if the cache misses (rare — only after a
// concurrent invalidation that hasn't republished yet).
func (m *gormManager) Get(ctx context.Context, appID string) (*RuntimeApp, error) {
	snap := m.readSnapshot()
	if ra, ok := snap.apps[appID]; ok {
		return ra, nil
	}
	// Cache miss : check the DB. If the row exists and is enabled,
	// load from disk and republish. If not, ErrAppNotFound / ErrAppDisabled.
	var row models.App
	if err := m.cfg.DB.WithContext(ctx).First(&row, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrAppNotFound
		}
		return nil, err
	}
	if !row.Enabled {
		return nil, ErrAppDisabled
	}
	ra, err := m.loadFromDisk(&row)
	if err != nil {
		return nil, err
	}
	m.swapSnapshot(appID, ra)
	return ra, nil
}

// GetManifest returns the compiled AppDefinition for an app — useful
// for the marketplace UI that wants the full tool catalogue without
// constructing a RuntimeApp.
func (m *gormManager) GetManifest(ctx context.Context, appID string) (*schema.AppDefinition, error) {
	ra, err := m.Get(ctx, appID)
	if err != nil {
		return nil, err
	}
	return ra.Definition, nil
}

// Bootstrap loads every enabled app from the DB and populates the
// snapshot. Apps whose dgc file is missing or fails to decode are
// logged and skipped — the daemon never panics on a single broken
// app. Returns nil even if some apps failed (use the logs to surface).
func (m *gormManager) Bootstrap(ctx context.Context) error {
	// Install any bundled built-in app the DB doesn't know yet, so a fresh database
	// comes up with working apps. Runs before the load below picks up its DB row.
	m.seedBuiltins(ctx)

	var rows []models.App
	if err := m.cfg.DB.WithContext(ctx).Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return fmt.Errorf("appmgr: bootstrap query: %w", err)
	}
	next := &snapshot{apps: make(map[string]*RuntimeApp, len(rows))}
	loaded := 0
	for i := range rows {
		row := &rows[i]
		ra, err := m.loadFromDisk(row)
		if err != nil {
			m.cfg.Logger.Warn("appmgr: bootstrap skipped broken app",
				slog.String("app_id", row.AppID),
				slog.String("err", err.Error()),
			)
			continue
		}
		next.apps[row.AppID] = ra
		loaded++
	}

	// Auto-discover apps that are INSTALLED ON DISK (have a compiled app.dgc)
	// but have no DB row — e.g. after the metadata DB was reset/cleared while
	// the bundles survived (both live under the same root, but the DB is the
	// piece that gets wiped). Each is registered enabled + loaded, so an
	// installed app is always available at startup without a re-install. An app
	// that already has a DB row is left to the DB's authority, so an explicit
	// Disable is respected and a re-discovery never resurrects it.
	discovered := m.reconcileDiskApps(ctx, next)

	m.snap.Store(next)
	m.cfg.Logger.Info("appmgr: bootstrap done",
		slog.Int("enabled_in_db", len(rows)),
		slog.Int("discovered_on_disk", discovered),
		slog.Int("loaded", loaded+discovered),
	)
	return nil
}

// reconcileDiskApps scans the install root for app bundles (a directory holding
// a decodable app.dgc) that the DB doesn't know about, registers them (enabled),
// and adds them to the snapshot being built. Returns the count discovered.
// Every failure is logged and skipped — a single bad bundle never aborts boot.
func (m *gormManager) reconcileDiskApps(ctx context.Context, next *snapshot) int {
	if m.cfg.Root == "" {
		return 0
	}
	entries, err := os.ReadDir(m.cfg.Root)
	if err != nil {
		return 0 // no install root yet : nothing to discover
	}

	// Every app_id the DB already knows (enabled OR disabled) is off-limits :
	// the DB is authoritative for those, so we never override a Disable.
	known := make(map[string]struct{})
	var ids []string
	if err := m.cfg.DB.WithContext(ctx).Model(&models.App{}).Pluck("app_id", &ids).Error; err == nil {
		for _, id := range ids {
			known[id] = struct{}{}
		}
	}

	now := time.Now().UTC()
	discovered := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		appID := e.Name()
		if _, ok := next.apps[appID]; ok {
			continue // already loaded from the DB this boot
		}
		if _, ok := known[appID]; ok {
			continue // DB knows it (e.g. disabled) : respect the DB
		}

		f, err := os.Open(m.dgcPath(appID))
		if err != nil {
			continue // no compiled bundle here : not an installed app, just a stray dir
		}
		art, derr := codegen.Decode(f)
		_ = f.Close()
		if derr != nil {
			m.cfg.Logger.Warn("appmgr: skip undecodable disk app",
				slog.String("app_id", appID), slog.String("err", derr.Error()))
			continue
		}
		if art.Definition == nil || art.Definition.App.AppID != appID {
			continue // malformed / mislabelled bundle
		}
		def := art.Definition

		row := models.App{
			AppID:       appID,
			Name:        firstNonEmpty(def.App.Name, appID),
			Version:     def.App.Version,
			Description: def.App.Description,
			Category:    string(def.App.Category),
			Author:      def.App.Author,
			Icon:        def.App.Icon,
			Color:       def.App.Color,
			Enabled:     true,
			InstalledAt: now,
			UpdatedAt:   now,
		}
		// Insert only if absent — a concurrent install racing us keeps its row.
		if err := m.cfg.DB.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "app_id"}},
			DoNothing: true,
		}).Create(&row).Error; err != nil {
			m.cfg.Logger.Warn("appmgr: disk app register failed",
				slog.String("app_id", appID), slog.String("err", err.Error()))
			continue
		}

		next.apps[appID] = &RuntimeApp{
			Meta:       metaFromRow(&row),
			Definition: def,
			BundleDir:  m.bundleDir(appID),
		}
		discovered++
		m.cfg.Logger.Info("appmgr: auto-loaded installed app from disk",
			slog.String("app_id", appID), slog.String("version", def.App.Version))
	}
	return discovered
}

// CheckUpdate compares the row's Version against the hub's latest.
// Returns a populated UpdateInfo with UpdateAvailable=false for
// non-hub-installed apps (no hub source recorded V1 — we treat
// missing source as "no update info available").
func (m *gormManager) CheckUpdate(ctx context.Context, appID, userJWT string) (*UpdateInfo, error) {
	var row models.App
	if err := m.cfg.DB.WithContext(ctx).First(&row, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrAppNotFound
		}
		return nil, err
	}
	// V1 : we don't persist the source URI per app, so we attempt a
	// hub lookup by "digitorn/{app_id}" convention. If the hub returns
	// a 404, we report no update available.
	out := &UpdateInfo{AppID: appID, CurrentVersion: row.Version}
	client := m.hubClient()
	v, err := m.hubLatest(ctx, client, "digitorn", appID, userJWT)
	if err != nil {
		// Treat any hub error as "no info" — not a failure of the API.
		m.cfg.Logger.Debug("appmgr: hub check-update miss",
			slog.String("app_id", appID), slog.String("err", err.Error()))
		return out, nil
	}
	out.LatestVersion = v
	out.UpdateAvailable = v != row.Version
	return out, nil
}

// loadFromDisk reads {Root}/{app_id}/app.dgc and produces a RuntimeApp.
// The compiled bytecode is the source of truth at boot — we don't
// re-read the YAML here.
func (m *gormManager) loadFromDisk(row *models.App) (*RuntimeApp, error) {
	dir := m.bundleDir(row.AppID)
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("install dir missing: %w", err)
	}
	dgc := m.dgcPath(row.AppID)
	f, err := os.Open(dgc)
	if err != nil {
		return nil, fmt.Errorf("open app.dgc: %w", err)
	}
	defer f.Close()
	art, err := codegen.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode app.dgc: %w", err)
	}
	if art.Definition == nil {
		return nil, fmt.Errorf("app.dgc has nil Definition")
	}
	if art.Definition.App.AppID != row.AppID {
		return nil, fmt.Errorf("app.dgc app_id %q != row %q", art.Definition.App.AppID, row.AppID)
	}
	return &RuntimeApp{
		Meta:       metaFromRow(row),
		Definition: art.Definition,
		BundleDir:  dir,
	}, nil
}

// Silence "unused" complaints for net/http import on platforms where
// the linker is aggressive about unused stdlib pulls.
var _ = http.MethodGet
