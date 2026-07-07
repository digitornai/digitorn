package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// officeConverter renders office documents (pptx/docx/xlsx/…) to PDF via a
// headless LibreOffice, for high-fidelity read-only preview. It is designed to
// NEVER block or slow the daemon, which serves hundreds of sessions:
//
//   - The HTTP handler never converts inline. `request` only does a fast cache
//     check and, at most, schedules a background goroutine — it returns
//     immediately with a status the client polls on.
//   - Concurrency is hard-capped by a semaphore (a couple of soffice processes
//     max); distinct files beyond `maxQueue` are refused (convBusy) so the
//     client falls back to the pure-JS viewer instead of piling up work.
//   - Each conversion is single-flighted (one per file), time-bounded (soffice
//     killed past the timeout), and uses an isolated user profile so parallel
//     soffice instances don't serialize on a shared lock.
//   - Results are cached content-addressed (path+mtime+size) → converted once,
//     served instantly thereafter. Recent failures are remembered briefly so a
//     broken file doesn't get hammered.
type officeConverter struct {
	bin      string // soffice/libreoffice path; "" when unavailable
	cacheDir string
	logger   *slog.Logger
	timeout  time.Duration

	sem chan struct{} // bounded concurrent conversions

	mu       sync.Mutex
	inflight map[string]struct{}   // keys currently scheduled/running
	failed   map[string]time.Time  // key -> last failure (short cooldown)
	queued   int                   // scheduled-but-unfinished
	maxQueue int
}

type convResult int

const (
	convReady       convResult = iota // PDF is cached and ready
	convPending                       // scheduled/running — poll again
	convBusy                          // queue full — fall back
	convError                         // recently failed — fall back
	convUnavailable                   // no LibreOffice — fall back
)

// officeExts are the formats we route through LibreOffice.
var officeExts = map[string]bool{
	".pptx": true, ".ppt": true, ".odp": true,
	".docx": true, ".doc": true, ".odt": true, ".rtf": true,
	".xlsx": true, ".xls": true, ".ods": true,
}

func newOfficeConverter(cacheDir string, logger *slog.Logger) *officeConverter {
	bin := ""
	for _, name := range []string{"soffice", "libreoffice"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	_ = os.MkdirAll(cacheDir, 0o755)
	return &officeConverter{
		bin:      bin,
		cacheDir: cacheDir,
		logger:   logger,
		timeout:  60 * time.Second,
		sem:      make(chan struct{}, 2), // ≤2 LibreOffice at once, whole daemon
		inflight: map[string]struct{}{},
		failed:   map[string]time.Time{},
		maxQueue: 16,
	}
}

func (c *officeConverter) available() bool { return c != nil && c.bin != "" }

func convertibleToPDF(name string) bool {
	return officeExts[strings.ToLower(filepath.Ext(name))]
}

func (c *officeConverter) cacheKey(abs string, fi os.FileInfo) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", abs, fi.ModTime().UnixNano(), fi.Size())))
	return hex.EncodeToString(h[:])
}

func (c *officeConverter) pdfPath(key string) string {
	return filepath.Join(c.cacheDir, key+".pdf")
}

// request is NON-BLOCKING. It returns the cached PDF path when ready, otherwise
// a status telling the caller to poll (convPending) or give up (convBusy /
// convError / convUnavailable). At most it schedules one background goroutine.
func (c *officeConverter) request(abs string, fi os.FileInfo) (string, convResult) {
	if !c.available() {
		return "", convUnavailable
	}
	key := c.cacheKey(abs, fi)
	out := c.pdfPath(key)
	if st, err := os.Stat(out); err == nil && st.Size() > 0 {
		return out, convReady
	}

	c.mu.Lock()
	if _, running := c.inflight[key]; running {
		c.mu.Unlock()
		return "", convPending
	}
	if t, ok := c.failed[key]; ok {
		if time.Since(t) < 30*time.Second {
			c.mu.Unlock()
			return "", convError
		}
		delete(c.failed, key)
	}
	if c.queued >= c.maxQueue {
		c.mu.Unlock()
		return "", convBusy
	}
	c.inflight[key] = struct{}{}
	c.queued++
	c.mu.Unlock()

	go c.run(key, abs, out)
	return "", convPending
}

func (c *officeConverter) run(key, src, out string) {
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.queued--
		c.mu.Unlock()
	}()

	// Bounded concurrency — extra work parks here, never spawns unbounded soffice.
	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	// Everything happens inside one staging dir: LibreOffice reads a COPY of the
	// source and writes there. Staging is mandatory for a sandboxed (snap)
	// LibreOffice — it can't read the daemon's hidden data dir nor the private
	// /tmp; a non-hidden $HOME base is the only thing its home interface grants.
	// Native/server LibreOffice is unrestricted, so /tmp is used there.
	tmp, err := os.MkdirTemp(c.stagingBase(), "conv-")
	if err != nil {
		c.markFailed(key, err, nil)
		return
	}
	defer os.RemoveAll(tmp)

	staged := filepath.Join(tmp, "input"+strings.ToLower(filepath.Ext(src)))
	if err := copyFileContents(src, staged); err != nil {
		c.markFailed(key, err, nil)
		return
	}

	// Isolated profile so parallel soffice instances don't block on one lock.
	profile := "-env:UserInstallation=file://" + filepath.Join(tmp, "profile")
	cmd := exec.CommandContext(ctx, c.bin,
		"--headless", "--norestore", "--nolockcheck", "--nodefault",
		profile, "--convert-to", "pdf", "--outdir", tmp, staged)
	if outb, err := cmd.CombinedOutput(); err != nil {
		c.markFailed(key, err, outb)
		return
	}

	produced := filepath.Join(tmp, "input.pdf")
	data, err := os.ReadFile(produced)
	if err != nil {
		c.markFailed(key, err, nil)
		return
	}
	// Atomic publish so a concurrent reader never sees a half-written PDF.
	tmpOut := out + ".tmp"
	if err := os.WriteFile(tmpOut, data, 0o644); err != nil {
		c.markFailed(key, err, nil)
		return
	}
	if err := os.Rename(tmpOut, out); err != nil {
		_ = os.Remove(tmpOut)
		c.markFailed(key, err, nil)
	}
}

// serveOfficePDF answers GET …/workspace/files/{path}?as=pdf. It never blocks
// on the conversion: cached → the PDF; otherwise a status the client polls
// (202) or falls back on (501/503/502 → pure-JS viewer).
func (d *Daemon) serveOfficePDF(w http.ResponseWriter, r *http.Request, abs string, fi os.FileInfo) {
	if !convertibleToPDF(abs) {
		writeError(w, http.StatusBadRequest, "not_convertible", "not an office document")
		return
	}
	path, status := d.officeConverter.request(abs, fi)
	switch status {
	case convReady:
		f, err := os.Open(path)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "workspace_error", err.Error())
			return
		}
		defer f.Close()
		name := filepath.Base(abs) + ".pdf"
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", "inline; filename=\""+name+"\"")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, name, fi.ModTime(), f)
	case convPending:
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "converting"})
	case convBusy:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "busy"})
	case convError:
		writeJSON(w, http.StatusBadGateway, map[string]any{"status": "failed"})
	default: // convUnavailable
		writeJSON(w, http.StatusNotImplemented, map[string]any{"status": "unavailable"})
	}
}

func (c *officeConverter) markFailed(key string, err error, out []byte) {
	if c.logger != nil {
		c.logger.Warn("office->pdf conversion failed", "err", err, "soffice_out", strings.TrimSpace(string(out)))
	}
	c.mu.Lock()
	c.failed[key] = time.Now()
	c.mu.Unlock()
}

// stagingBase picks a scratch base LibreOffice can actually write to. A snap
// build is confined to non-hidden files under $HOME (its `home` interface), so
// a visible $HOME dir is the only universally-writable choice; a native build
// is unrestricted, so the standard temp dir is used.
func (c *officeConverter) stagingBase() string {
	if strings.Contains(c.bin, "/snap/") {
		if home, err := os.UserHomeDir(); err == nil {
			d := filepath.Join(home, "digitorn-office-tmp")
			if os.MkdirAll(d, 0o755) == nil {
				return d
			}
		}
	}
	return os.TempDir()
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
