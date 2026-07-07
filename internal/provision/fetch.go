// Package provision downloads, verifies and installs the external system
// binaries an app declares under `requirements:` — out-of-band from any request,
// consent-gated, and never blocking the daemon. See provision.go for the
// orchestration; this file is the pure fetch/verify/extract mechanism.
package provision

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// progressFn reports download progress; total is -1 when unknown.
type progressFn func(done, total int64)

// download streams url to dst, returning the lowercase-hex SHA-256 of the bytes
// written. Progress is reported (throttled by the caller's fn) as it streams.
func download(ctx context.Context, hc *http.Client, url, dst string, onProgress progressFn) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return "", werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if onProgress != nil {
				onProgress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// install lays the artifact out under destDir and returns the absolute path to
// the executable to expose. `src` is the downloaded file, `format` its kind, and
// `binPath` the path (within an archive) to the executable — or, for a raw
// binary, the filename to give it.
func install(format, src, destDir, binPath string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	switch normalizeFormat(format) {
	case "binary":
		out := filepath.Join(destDir, filepath.Base(orDefault(binPath, filepath.Base(src))))
		if err := copyFile(src, out, 0o755); err != nil {
			return "", err
		}
		return out, nil
	case "gz":
		out := filepath.Join(destDir, filepath.Base(orDefault(binPath, strings.TrimSuffix(filepath.Base(src), ".gz"))))
		if err := gunzipFile(src, out); err != nil {
			return "", err
		}
		if err := os.Chmod(out, 0o755); err != nil {
			return "", err
		}
		return out, nil
	case "tar.gz":
		if err := extractTarGz(src, destDir); err != nil {
			return "", err
		}
		return resolveBin(destDir, binPath)
	case "zip":
		if err := extractZip(src, destDir); err != nil {
			return "", err
		}
		return resolveBin(destDir, binPath)
	default:
		return "", fmt.Errorf("unknown artifact format %q", format)
	}
}

func normalizeFormat(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "", "binary", "bin", "raw":
		return "binary"
	case "gz", "gzip":
		return "gz"
	case "tar.gz", "tgz", "targz":
		return "tar.gz"
	case "zip":
		return "zip"
	default:
		return strings.ToLower(strings.TrimSpace(f))
	}
}

// resolveBin locates the executable inside an extracted tree and chmods it.
// binPath, when given, is the relative path within destDir; otherwise the first
// regular file whose basename matches is used (best effort — callers should set
// path for multi-file archives).
func resolveBin(destDir, binPath string) (string, error) {
	if binPath != "" {
		p := filepath.Join(destDir, filepath.FromSlash(binPath))
		if !within(destDir, p) {
			return "", fmt.Errorf("bin path escapes archive: %q", binPath)
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("declared bin %q not found after extract: %w", binPath, err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return "", err
		}
		return p, nil
	}
	// No path declared: pick the sole executable-looking file if unambiguous.
	var found string
	err := filepath.Walk(destDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if info.Mode()&0o111 != 0 || !strings.Contains(filepath.Base(p), ".") {
			if found != "" {
				return fmt.Errorf("multiple candidate binaries; declare `path`")
			}
			found = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no executable found after extract; declare `path`")
	}
	if err := os.Chmod(found, 0o755); err != nil {
		return "", err
	}
	return found, nil
}

func extractTarGz(src, destDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
		if !within(destDir, target) {
			return fmt.Errorf("tar entry escapes dest: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			if err := writeReg(target, tr, mode); err != nil {
				return err
			}
		}
	}
}

func extractZip(src, destDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		target := filepath.Join(destDir, filepath.FromSlash(zf.Name))
		if !within(destDir, target) {
			return fmt.Errorf("zip entry escapes dest: %q", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		mode := zf.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		werr := writeReg(target, rc, mode)
		rc.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}

func writeReg(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func gunzipFile(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return writeReg(dst, gz, 0o755)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeReg(dst, in, mode)
}

// within reports whether target stays inside base (path-traversal guard).
func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
}
