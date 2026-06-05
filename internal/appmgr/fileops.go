package appmgr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// copyDirVerbatim copies every file and subdirectory from src to dst,
// preserving the relative tree structure. The destination is wiped
// first so the operation is "set to source state" rather than "merge".
// Symlinks inside src are followed (file contents copied), never
// reproduced, so the destination never references anything outside
// dst. Hidden files (starting with ".") are copied normally.
func copyDirVerbatim(src, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("abs source: %w", err)
	}
	info, err := os.Stat(srcAbs)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", srcAbs)
	}

	// Wipe destination if it exists, then recreate.
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("wipe dest: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir dest: %w", err)
	}

	return filepath.Walk(srcAbs, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)

		if fi.IsDir() {
			return os.MkdirAll(target, fi.Mode().Perm())
		}
		// Regular file (or symlink to one — filepath.Walk follows
		// symlinks by default).
		return copyFile(path, target, fi.Mode().Perm())
	})
}

// copyFile copies src to dst as a regular file with the given perms.
func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copy bytes: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to path via a tmp file + rename. The
// dest is created if missing (parents not — caller mkdirs).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// bundleDir returns the absolute install directory for an app.
func (m *gormManager) bundleDir(appID string) string {
	return filepath.Join(m.cfg.Root, appID)
}

// dgcPath returns the absolute path to the compiled bundle file.
func (m *gormManager) dgcPath(appID string) string {
	return filepath.Join(m.bundleDir(appID), "app.dgc")
}

// validateAppID rejects values that would create unsafe paths. We
// strictly accept [a-zA-Z0-9_-] with length 1..128, matching the
// compiler's app_id regex. The pure-filename constraint blocks "..",
// "/", "\", "." (current dir), and absolute paths.
func validateAppID(id string) error {
	if id == "" || len(id) > 128 {
		return fmt.Errorf("appmgr: app_id must be 1..128 chars, got %d", len(id))
	}
	if strings.Contains(id, "/") || strings.Contains(id, "\\") ||
		strings.Contains(id, "..") || id == "." || id[0] == '.' {
		return fmt.Errorf("appmgr: app_id %q contains forbidden character", id)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("appmgr: app_id %q has invalid char %q", id, r)
	}
	return nil
}
