package appmgr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/compiler/codegen"
	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// Enable flips the row's Enabled flag to true and republishes the
// RuntimeApp in the snapshot. Idempotent.
func (m *gormManager) Enable(ctx context.Context, appID string) error {
	lock := m.lockFor(appID)
	lock.Lock()
	defer lock.Unlock()

	var row models.App
	if err := m.cfg.DB.WithContext(ctx).First(&row, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrAppNotFound
		}
		return err
	}
	if !row.Enabled {
		row.Enabled = true
		row.UpdatedAt = time.Now().UTC()
		if err := m.cfg.DB.WithContext(ctx).Save(&row).Error; err != nil {
			return err
		}
	}

	ra, err := m.loadFromDisk(&row)
	if err != nil {
		return fmt.Errorf("appmgr: enable load: %w", err)
	}
	m.swapSnapshot(appID, ra)
	m.cfg.Logger.Info("appmgr: app enabled", slog.String("app_id", appID))
	return nil
}

// Disable flips Enabled=false and removes the entry from the snapshot.
// The on-disk install dir + app.dgc stay intact for fast re-enable.
func (m *gormManager) Disable(ctx context.Context, appID string) error {
	lock := m.lockFor(appID)
	lock.Lock()
	defer lock.Unlock()

	var row models.App
	if err := m.cfg.DB.WithContext(ctx).First(&row, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrAppNotFound
		}
		return err
	}
	if row.Enabled {
		row.Enabled = false
		row.UpdatedAt = time.Now().UTC()
		if err := m.cfg.DB.WithContext(ctx).Save(&row).Error; err != nil {
			return err
		}
	}
	m.swapSnapshot(appID, nil)
	m.cfg.Logger.Info("appmgr: app disabled", slog.String("app_id", appID))
	return nil
}

// SetBYOK flips the row's BYOK routing flag and republishes the
// snapshot so the next runtime turn picks up the change without a
// daemon restart. Idempotent : a no-op when the flag is already in
// the desired state. Works on enabled and disabled apps alike — for a
// disabled app the snapshot is untouched (it's not in there), but the
// DB row is updated so re-enabling picks up the new value.
func (m *gormManager) SetBYOK(ctx context.Context, appID string, enabled bool) error {
	lock := m.lockFor(appID)
	lock.Lock()
	defer lock.Unlock()

	var row models.App
	if err := m.cfg.DB.WithContext(ctx).First(&row, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrAppNotFound
		}
		return err
	}
	if row.BYOK == enabled {
		return nil
	}
	row.BYOK = enabled
	row.UpdatedAt = time.Now().UTC()
	if err := m.cfg.DB.WithContext(ctx).Save(&row).Error; err != nil {
		return err
	}

	if row.Enabled {
		ra, err := m.loadFromDisk(&row)
		if err != nil {
			return fmt.Errorf("appmgr: setbyok load: %w", err)
		}
		m.swapSnapshot(appID, ra)
	}

	m.cfg.Logger.Info("appmgr: app BYOK updated",
		slog.String("app_id", appID),
		slog.Bool("byok", enabled),
	)
	return nil
}

// Reload recompiles the app from its on-disk source (when the operator
// edited app.yaml by hand) and replaces app.dgc + snapshot. The DB row
// metadata is refreshed too (name / version / description / etc).
func (m *gormManager) Reload(ctx context.Context, appID string) error {
	lock := m.lockFor(appID)
	lock.Lock()
	defer lock.Unlock()

	var row models.App
	if err := m.cfg.DB.WithContext(ctx).First(&row, "app_id = ?", appID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrAppNotFound
		}
		return err
	}
	dir := m.bundleDir(appID)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("appmgr: install dir missing for %q: %w", appID, err)
	}

	result, err := m.cfg.Compiler.Compile(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCompileFailed, err)
	}
	if !result.OK() {
		errs := result.Diagnostics.Errors()
		msgs := make([]string, len(errs))
		for i, d := range errs {
			msgs[i] = fmt.Sprintf("%s [%s]", d.Message, d.Code)
		}
		return fmt.Errorf("%w: %s", ErrCompileFailed, strings.Join(msgs, "; "))
	}
	if result.Definition.App.AppID != appID {
		return fmt.Errorf("appmgr: reloaded yaml app_id=%q differs from row %q",
			result.Definition.App.AppID, appID)
	}
	artifact, err := m.cfg.Compiler.Build(result)
	if err != nil {
		return fmt.Errorf("appmgr: build artifact: %w", err)
	}
	dgcBytes, err := codegen.EncodeBytes(artifact)
	if err != nil {
		return fmt.Errorf("appmgr: encode dgc: %w", err)
	}
	if err := writeFileAtomic(m.dgcPath(appID), dgcBytes, 0o644); err != nil {
		return fmt.Errorf("appmgr: write dgc: %w", err)
	}

	// Refresh denormalized DB metadata.
	row.Name = firstNonEmpty(result.Definition.App.Name, appID)
	row.ShortName = result.Definition.App.ShortName
	row.Version = result.Definition.App.Version
	row.Description = result.Definition.App.Description
	row.Category = string(result.Definition.App.Category)
	row.Author = result.Definition.App.Author
	row.Icon = result.Definition.App.Icon
	row.Color = result.Definition.App.Color
	row.UpdatedAt = time.Now().UTC()
	if err := m.cfg.DB.WithContext(ctx).Save(&row).Error; err != nil {
		return err
	}

	if row.Enabled {
		ra := &RuntimeApp{
			Meta:       metaFromRow(&row),
			Definition: artifact.Definition,
			BundleDir:  dir,
		}
		m.swapSnapshot(appID, ra)
	}
	m.cfg.Logger.Info("appmgr: app reloaded", slog.String("app_id", appID))
	return nil
}

// Uninstall removes the row and the install dir. `purge` is for the
// caller : the App Manager itself doesn't reach into the session
// store. The REST handler will use it to decide whether to also clean
// sessionstore paths for this app_id.
func (m *gormManager) Uninstall(ctx context.Context, appID string, purge bool) error {
	lock := m.lockFor(appID)
	lock.Lock()
	defer lock.Unlock()

	if err := m.cfg.DB.WithContext(ctx).Delete(&models.App{}, "app_id = ?", appID).Error; err != nil {
		return err
	}
	dir := m.bundleDir(appID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("appmgr: rm install dir: %w", err)
	}
	m.swapSnapshot(appID, nil)
	m.cfg.Logger.Info("appmgr: app uninstalled",
		slog.String("app_id", appID),
		slog.Bool("purge", purge),
	)
	return nil
}
