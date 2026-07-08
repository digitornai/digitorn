package appmgr

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

// builtinFS holds the app sources SHIPPED WITH the daemon : builtins/<app_id>/app.yaml
// (+ any sibling assets). Embedded into the binary so a fresh install has working
// apps with no manual step. Add a built-in by dropping a directory under builtins/.
//
//go:embed all:builtins
var builtinFS embed.FS

// builtinAppNames lists the bundled app ids (each builtins/<id>/ holding an app.yaml).
func builtinAppNames() []string {
	entries, err := builtinFS.ReadDir("builtins")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// fetchBuiltin extracts a built-in app's embedded source into a fresh temp dir that
// the install path compiles like any other local source. The caller removes it via
// fetchInfo.cleanup.
func (m *gormManager) fetchBuiltin(name string) (string, error) {
	sub, err := fs.Sub(builtinFS, "builtins/"+name)
	if err != nil {
		return "", fmt.Errorf("%w: no built-in app %q", ErrBadSource, name)
	}
	if _, err := fs.Stat(sub, "app.yaml"); err != nil {
		return "", fmt.Errorf("%w: built-in %q has no app.yaml", ErrBadSource, name)
	}
	dir, err := os.MkdirTemp("", "digitorn-builtin-"+name+"-*")
	if err != nil {
		return "", err
	}
	if err := copyEmbedTree(sub, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// seedBuiltins seeds built-ins at boot. Channel "server" routes via
// channels.yaml (re-install server apps); "" keeps seed-if-missing.
func (m *gormManager) seedBuiltins(ctx context.Context) {
	if m.cfg.Channel == "server" {
		m.seedServerChannel(ctx)
		return
	}
	for _, name := range builtinAppNames() {
		var count int64
		if err := m.cfg.DB.WithContext(ctx).Model(&models.App{}).Where("app_id = ?", name).Count(&count).Error; err != nil || count > 0 {
			continue
		}
		if _, err := m.Install(ctx, "builtin://"+name, ""); err != nil {
			m.cfg.Logger.Warn("appmgr: built-in seed failed", slog.String("app", name), slog.String("err", err.Error()))
			continue
		}
		m.cfg.Logger.Info("appmgr: seeded built-in app", slog.String("app", name))
	}
}

// seedServerChannel (re-)installs server-channel built-ins; skips apps already
// installed, enabled and at the bundled version.
func (m *gormManager) seedServerChannel(ctx context.Context) {
	routing := loadChannels()
	for _, name := range builtinAppNames() {
		if !routing[name].Server {
			continue
		}
		var existing models.App
		err := m.cfg.DB.WithContext(ctx).Where("app_id = ?", name).First(&existing).Error
		if err == nil && existing.Enabled && existing.Version == builtinVersion(name) {
			continue // installed, enabled, current
		}
		if _, err := m.Install(ctx, "builtin://"+name, ""); err != nil {
			m.cfg.Logger.Warn("appmgr: server built-in seed failed", slog.String("app", name), slog.String("err", err.Error()))
			continue
		}
		m.cfg.Logger.Info("appmgr: seeded/updated server built-in", slog.String("app", name))
	}
}

func copyEmbedTree(src fs.FS, dst string) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, p)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
