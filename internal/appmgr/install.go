package appmgr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/digitornai/digitorn/internal/compiler/codegen"
	"github.com/digitornai/digitorn/internal/persistence/models"
)

// Install resolves the source, compiles it, persists it, and publishes
// the new RuntimeApp into the snapshot. See Manager.Install for the
// source URI conventions.
func (m *gormManager) Install(ctx context.Context, source, userJWT string) (*App, error) {
	srcDir, fetchInfo, err := m.fetchSource(ctx, source, userJWT)
	if err != nil {
		return nil, err
	}
	defer fetchInfo.cleanup()

	// Read app.yaml and compile to get the app_id + Artifact.
	yamlPath := filepath.Join(srcDir, "app.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		return nil, ErrSourceMissingYAML
	}
	result, err := m.cfg.Compiler.Compile(srcDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCompileFailed, err)
	}
	if !result.OK() {
		// Surface the first few error diagnostics so callers (CLI, HTTP
		// install endpoint, live tests) get something actionable instead
		// of the bare count.
		errs := result.Diagnostics.Errors()
		messages := make([]string, 0, len(errs))
		for _, d := range errs {
			messages = append(messages, fmt.Sprintf("[%s] %s", d.Code, d.Message))
		}
		preview := strings.Join(messages, "; ")
		return nil, fmt.Errorf("%w: %d diagnostic(s): %s", ErrCompileFailed, len(errs), preview)
	}
	appID := result.Definition.App.AppID
	if err := validateAppID(appID); err != nil {
		return nil, err
	}

	// Strict-folder-name rule applies only to LOCAL sources where the
	// caller picked the dir : we reject if basename(srcDir) != app_id.
	// Hub / builtin sources are fetched into a sandbox so basename
	// happens to be a temp name — we skip the check there.
	if fetchInfo.kind == sourceLocal {
		want := filepath.Base(strings.TrimRight(srcDir, string(os.PathSeparator)))
		if want != appID {
			return nil, fmt.Errorf("%w: dir=%q yaml app_id=%q", ErrAppIDMismatch, want, appID)
		}
	}

	// Build the .dgc artifact (deterministic from the compiled Result).
	artifact, err := m.cfg.Compiler.Build(result)
	if err != nil {
		return nil, fmt.Errorf("appmgr: build artifact: %w", err)
	}
	dgcBytes, err := codegen.EncodeBytes(artifact)
	if err != nil {
		return nil, fmt.Errorf("appmgr: encode bytecode: %w", err)
	}

	// Acquire the per-app lock from here on : same app can't be
	// installed twice concurrently, but other apps are unaffected.
	lock := m.lockFor(appID)
	lock.Lock()
	defer lock.Unlock()

	// Copy the entire source tree into {Root}/{app_id}/. This wipes
	// any pre-existing dir so the install is set-to-source semantics.
	dest := m.bundleDir(appID)
	if err := copyDirVerbatim(srcDir, dest); err != nil {
		return nil, fmt.Errorf("appmgr: copy source: %w", err)
	}

	// Write the compiled bytecode next to the source files.
	if err := writeFileAtomic(m.dgcPath(appID), dgcBytes, 0o644); err != nil {
		_ = os.RemoveAll(dest) // best-effort cleanup
		return nil, fmt.Errorf("appmgr: write dgc: %w", err)
	}

	// Upsert the DB row. Denormalized metadata for fast list queries.
	now := time.Now().UTC()
	row := models.App{
		AppID:       appID,
		Name:        firstNonEmpty(result.Definition.App.Name, appID),
		ShortName:   result.Definition.App.ShortName,
		Version:     result.Definition.App.Version,
		Description: result.Definition.App.Description,
		Category:    string(result.Definition.App.Category),
		Author:      result.Definition.App.Author,
		Icon:        result.Definition.App.Icon,
		Color:       result.Definition.App.Color,
		Enabled:     true,
		InstalledAt: now,
		UpdatedAt:   now,
	}
	if err := m.cfg.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "app_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name", "short_name", "version", "description", "category", "author", "icon", "color",
			"enabled", "updated_at",
		}),
	}).Create(&row).Error; err != nil {
		_ = os.RemoveAll(dest)
		return nil, fmt.Errorf("appmgr: upsert row: %w", err)
	}

	// Re-read the row : the upsert preserves operator-owned columns
	// (BYOK, future feature flags) that are NOT in DoUpdates. The Go
	// `row` struct still holds the literal we tried to insert (zero
	// values for those columns), so without the re-read the snapshot
	// would clobber the operator's BYOK choice across re-installs.
	var fresh models.App
	if err := m.cfg.DB.WithContext(ctx).First(&fresh, "app_id = ?", appID).Error; err != nil {
		return nil, fmt.Errorf("appmgr: refetch row: %w", err)
	}

	// Republish snapshot.
	ra := &RuntimeApp{
		Meta:       metaFromRow(&fresh),
		Definition: artifact.Definition,
		BundleDir:  dest,
	}
	m.swapSnapshot(appID, ra)

	m.cfg.Logger.Info("appmgr: app installed",
		slog.String("app_id", appID),
		slog.String("version", fresh.Version),
		slog.String("source_kind", fetchInfo.kind.String()),
		slog.String("install_dir", dest),
		slog.Bool("byok", fresh.BYOK),
	)
	return metaFromRow(&fresh), nil
}

// Upgrade is a thin wrapper that delegates to Install — install is
// already an upsert that overwrites in place. We just validate that
// the app currently exists so the caller gets a clear 404 if not.
func (m *gormManager) Upgrade(ctx context.Context, appID, source, userJWT string) (*App, error) {
	var existing models.App
	if err := m.cfg.DB.WithContext(ctx).First(&existing, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrAppNotFound
		}
		return nil, err
	}
	out, err := m.Install(ctx, source, userJWT)
	if err != nil {
		return nil, err
	}
	if out.AppID != appID {
		// Source provided a different app_id than the row we're
		// upgrading — refuse to silently switch identity.
		return nil, fmt.Errorf("appmgr: upgrade target %q != source app_id %q", appID, out.AppID)
	}
	return out, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
