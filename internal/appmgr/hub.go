package appmgr

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// hubURIRegex matches "hub://publisher/package[@version]". Publisher
// and package are restricted to URL-safe slugs ; version is optional
// (omitted → resolve "latest" via the hub API).
var hubURIRegex = regexp.MustCompile(`^hub://([a-z0-9][a-z0-9_-]*)/([a-z0-9][a-z0-9_-]*)(?:@([a-zA-Z0-9._-]+))?$`)

// fetchHub downloads the tar.gz for a hub:// URI, extracts it into a
// fresh temp directory, and returns that directory's path. The caller
// is responsible for cleaning up the temp dir (handled via fetchInfo).
//
// The user's digitorn JWT is forwarded to the hub as Bearer for auth.
func (m *gormManager) fetchHub(ctx context.Context, source, userJWT string) (string, error) {
	publisher, pkg, version, err := parseHubURI(source)
	if err != nil {
		return "", err
	}

	client := m.hubClient()

	// Resolve "latest" if version omitted.
	if version == "" {
		v, err := m.hubLatest(ctx, client, publisher, pkg, userJWT)
		if err != nil {
			return "", fmt.Errorf("%w: resolve latest for %s/%s: %v", ErrHubFetch, publisher, pkg, err)
		}
		version = v
	}

	// GET .../packages/{publisher}/{package}/versions/{version}/download
	downloadURL := fmt.Sprintf("%s/api/v1/packages/%s/%s/versions/%s/download",
		strings.TrimRight(m.cfg.Hub.URL, "/"),
		url.PathEscape(publisher), url.PathEscape(pkg), url.PathEscape(version))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build request: %v", ErrHubFetch, err)
	}
	if userJWT != "" {
		req.Header.Set("Authorization", "Bearer "+userJWT)
	}
	req.Header.Set("Accept", "application/gzip,application/x-gzip,*/*")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrHubFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: hub returned HTTP %d", ErrHubFetch, resp.StatusCode)
	}

	// Cap the download size to the configured limit so a malicious
	// hub cannot exhaust local disk.
	limit := m.cfg.Hub.MaxArchiveBytes
	if limit <= 0 {
		limit = 100 * 1024 * 1024
	}
	body := io.LimitReader(resp.Body, limit+1)

	// Extract directly from the gzipped stream into a temp dir.
	tmp, err := os.MkdirTemp("", "digitorn-hub-*")
	if err != nil {
		return "", fmt.Errorf("%w: mkdtemp: %v", ErrHubFetch, err)
	}
	if err := extractTarGz(body, tmp, limit); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}

	// Unwrap a single top-level directory if present (matches hub
	// archive convention where everything is wrapped in
	// "publisher_pkg_version/").
	if wrapped := singleTopDir(tmp); wrapped != "" {
		flat, err := os.MkdirTemp("", "digitorn-hub-flat-*")
		if err != nil {
			_ = os.RemoveAll(tmp)
			return "", fmt.Errorf("%w: mkdtemp flat: %v", ErrHubFetch, err)
		}
		if err := copyDirVerbatim(filepath.Join(tmp, wrapped), flat); err != nil {
			_ = os.RemoveAll(tmp)
			_ = os.RemoveAll(flat)
			return "", fmt.Errorf("%w: flatten: %v", ErrHubFetch, err)
		}
		_ = os.RemoveAll(tmp)
		tmp = flat
	}

	// Sanity check : app.yaml must exist.
	if _, err := os.Stat(filepath.Join(tmp, "app.yaml")); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("%w: archive has no app.yaml", ErrHubFetch)
	}
	return tmp, nil
}

// hubLatest returns the latest version string for (publisher, package).
func (m *gormManager) hubLatest(ctx context.Context, client *http.Client, publisher, pkg, userJWT string) (string, error) {
	u := fmt.Sprintf("%s/api/v1/packages/%s/%s",
		strings.TrimRight(m.cfg.Hub.URL, "/"),
		url.PathEscape(publisher), url.PathEscape(pkg))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if userJWT != "" {
		req.Header.Set("Authorization", "Bearer "+userJWT)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var meta struct {
		LatestVersion string `json:"latest_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", err
	}
	if meta.LatestVersion == "" {
		return "", fmt.Errorf("hub response missing latest_version")
	}
	return meta.LatestVersion, nil
}

func (m *gormManager) hubClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !m.cfg.Hub.VerifySSL}, //nolint:gosec — VerifySSL is operator-controlled
	}
	return &http.Client{
		Transport: tr,
		Timeout:   m.cfg.Hub.Timeout,
	}
}

// parseHubURI parses "hub://publisher/package[@version]".
func parseHubURI(uri string) (publisher, pkg, version string, err error) {
	m := hubURIRegex.FindStringSubmatch(uri)
	if m == nil {
		return "", "", "", fmt.Errorf("%w: invalid hub URI %q", ErrBadSource, uri)
	}
	return m[1], m[2], m[3], nil
}

// extractTarGz extracts a gzipped tar stream into dest, rejecting any
// unsafe path (absolute, "..", outside dest, symlink, hardlink). The
// running total of decompressed bytes is checked against maxBytes so
// a tar bomb cannot fill the disk.
func extractTarGz(r io.Reader, dest string, maxBytes int64) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: gzip: %v", ErrHubFetch, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("%w: abs dest: %v", ErrHubFetch, err)
	}

	var written int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: tar: %v", ErrHubFetch, err)
		}
		name := hdr.Name
		if name == "" {
			continue
		}
		// Reject unsafe paths.
		if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
			return fmt.Errorf("%w: absolute path %q", ErrArchiveTraversal, name)
		}
		clean := filepath.Clean(name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: traversal %q", ErrArchiveTraversal, name)
		}
		target := filepath.Join(destAbs, clean)
		// Final path must stay inside destAbs.
		rel, err := filepath.Rel(destAbs, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%w: outside dest %q", ErrArchiveTraversal, name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("%w: mkdir %q: %v", ErrHubFetch, target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("%w: mkdir parent %q: %v", ErrHubFetch, target, err)
			}
			f, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return fmt.Errorf("%w: create %q: %v", ErrHubFetch, target, err)
			}
			// Bound copy by remaining budget.
			remaining := maxBytes - written
			if remaining <= 0 {
				f.Close()
				return ErrArchiveTooBig
			}
			n, err := io.CopyN(f, tr, remaining+1)
			f.Close()
			if err != nil && err != io.EOF {
				return fmt.Errorf("%w: copy: %v", ErrHubFetch, err)
			}
			written += n
			if written > maxBytes {
				return ErrArchiveTooBig
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("%w: link %q", ErrArchiveTraversal, name)
		default:
			// Skip char/block/fifo devices silently.
		}
	}
	return nil
}

// singleTopDir returns the basename of dir if dir contains exactly one
// entry and that entry is a directory ; otherwise "".
func singleTopDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return ""
	}
	return entries[0].Name()
}
