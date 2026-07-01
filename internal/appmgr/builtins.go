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

// seedBuiltins installs every bundled built-in app that the DB doesn't know yet, so
// a fresh database (new machine / reset clone) boots with working apps and no manual
// install step. An app already recorded in the DB — even if the user disabled it — is
// left untouched, so an explicit uninstall is never resurrected.
func (m *gormManager) seedBuiltins(ctx context.Context) {
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
