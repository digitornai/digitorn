package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// State is the lifecycle of one requirement on this host.
type State string

const (
	StateUnsupported State = "unsupported" // no artifact for this os/arch
	StateMissing     State = "missing"     // supported, not yet provisioned
	StateQueued      State = "queued"
	StateDownloading State = "downloading"
	StateVerifying   State = "verifying"
	StateInstalling  State = "installing"
	StateReady       State = "ready"
	StateFailed      State = "failed"
)

// Status is the client-facing state of a requirement.
type Status struct {
	ID          string `json:"id"`
	Bin         string `json:"bin"`
	Label       string `json:"label"`
	Description  string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	State       State  `json:"state"`
	Progress    int    `json:"progress"` // 0..100 (download %)
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	Error       string `json:"error,omitempty"`
}

// EmitFn delivers a progress event to the client viewing appID (best-effort;
// the client also polls the status endpoint, so events are an optimisation).
type EmitFn func(appID, event string, payload any)

type marker struct {
	SHA256      string `json:"sha256"`
	Exe         string `json:"exe"`
	InstalledAt string `json:"installed_at"`
}

// Provisioner owns the shared tools tree (~/.digitorn/tools) and a small pool of
// download workers. One instance per daemon.
type Provisioner struct {
	root   string // ~/.digitorn/tools
	binDir string // root/bin — prepended to the agent's PATH
	hc     *http.Client
	log    *slog.Logger
	emit   EmitFn

	mu     sync.Mutex
	status map[string]*Status // key: id@version
	jobs   chan job
}

type job struct {
	appID string
	req   schema.Requirement
	art   schema.PlatformArtifact
}

// New creates a Provisioner rooted at toolsRoot and starts its workers. emit may
// be nil (events disabled; clients poll instead).
func New(toolsRoot string, emit EmitFn, log *slog.Logger) *Provisioner {
	if log == nil {
		log = slog.Default()
	}
	p := &Provisioner{
		root:   toolsRoot,
		binDir: filepath.Join(toolsRoot, "bin"),
		hc:     &http.Client{}, // no total timeout; per-job ctx bounds it
		log:    log.With("component", "provision"),
		emit:   emit,
		status: map[string]*Status{},
		jobs:   make(chan job, 256),
	}
	_ = os.MkdirAll(p.binDir, 0o755)
	for i := 0; i < 2; i++ {
		go p.worker()
	}
	return p
}

// BinDir is the directory (holding symlinks to every provisioned executable)
// that must be prepended to the agent's PATH.
func (p *Provisioner) BinDir() string { return p.binDir }

func platformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

func key(r schema.Requirement) string { return r.ID + "@" + r.Version }

func (p *Provisioner) dirFor(r schema.Requirement) string {
	return filepath.Join(p.root, r.ID, r.Version)
}
func (p *Provisioner) markerPath(r schema.Requirement) string {
	return filepath.Join(p.dirFor(r), ".ok")
}

// artifactFor returns the artifact for this host, if the requirement supports it.
func artifactFor(r schema.Requirement) (schema.PlatformArtifact, bool) {
	if a, ok := r.Platforms[platformKey()]; ok {
		return a, true
	}
	return schema.PlatformArtifact{}, false
}

// ready reports whether the requirement is installed and its bin symlink resolves.
func (p *Provisioner) ready(r schema.Requirement) bool {
	if _, err := os.Stat(p.markerPath(r)); err != nil {
		return false
	}
	link := filepath.Join(p.binDir, r.EffectiveBin())
	if _, err := os.Stat(link); err != nil { // stat follows the symlink → target must exist
		return false
	}
	return true
}

// Statuses returns the current status of every requirement (for the consent UI).
func (p *Provisioner) Statuses(reqs []schema.Requirement) []Status {
	out := make([]Status, 0, len(reqs))
	for _, r := range reqs {
		art, supported := artifactFor(r)
		s := Status{
			ID: r.ID, Bin: r.EffectiveBin(), Label: r.EffectiveLabel(),
			Description: r.Description, Version: r.Version, SizeBytes: art.SizeBytes,
		}
		switch {
		case !supported:
			s.State = StateUnsupported
		case p.ready(r):
			s.State = StateReady
			s.Progress = 100
		default:
			// In-flight state (if any) wins over "missing".
			p.mu.Lock()
			if live, ok := p.status[key(r)]; ok {
				s.State, s.Progress, s.Error = live.State, live.Progress, live.Error
			} else {
				s.State = StateMissing
			}
			p.mu.Unlock()
		}
		out = append(out, s)
	}
	return out
}

// Missing returns the supported-but-not-ready requirements (what consent covers).
func (p *Provisioner) Missing(reqs []schema.Requirement) []schema.Requirement {
	var out []schema.Requirement
	for _, r := range reqs {
		if _, ok := artifactFor(r); !ok {
			continue
		}
		if !p.ready(r) {
			out = append(out, r)
		}
	}
	return out
}

// AllReady reports whether every SUPPORTED requirement is installed (unsupported
// ones are ignored — the host simply can't run them).
func (p *Provisioner) AllReady(reqs []schema.Requirement) bool {
	return len(p.Missing(reqs)) == 0
}

// Provision enqueues download jobs for every missing requirement. Idempotent and
// non-blocking: already-ready or in-flight requirements are skipped, and the call
// returns immediately (work happens on the worker pool).
func (p *Provisioner) Provision(appID string, reqs []schema.Requirement) {
	for _, r := range p.Missing(reqs) {
		art, _ := artifactFor(r)
		k := key(r)
		p.mu.Lock()
		if s, ok := p.status[k]; ok && (s.State == StateQueued || s.State == StateDownloading ||
			s.State == StateVerifying || s.State == StateInstalling) {
			p.mu.Unlock()
			continue // already in flight
		}
		p.status[k] = &Status{ID: r.ID, Bin: r.EffectiveBin(), Label: r.EffectiveLabel(),
			Version: r.Version, State: StateQueued, SizeBytes: art.SizeBytes}
		p.mu.Unlock()
		p.emitStatus(appID, r)
		select {
		case p.jobs <- job{appID: appID, req: r, art: art}:
		default:
			// Queue full (pathological): run detached so we never drop work.
			go p.run(job{appID: appID, req: r, art: art})
		}
	}
}

func (p *Provisioner) worker() {
	for j := range p.jobs {
		p.run(j)
	}
}

func (p *Provisioner) setState(r schema.Requirement, st State, progress int, errMsg string) {
	p.mu.Lock()
	s, ok := p.status[key(r)]
	if !ok {
		s = &Status{ID: r.ID, Bin: r.EffectiveBin(), Label: r.EffectiveLabel(), Version: r.Version}
		p.status[key(r)] = s
	}
	s.State, s.Error = st, errMsg
	if progress >= 0 {
		s.Progress = progress
	}
	p.mu.Unlock()
}

func (p *Provisioner) emitStatus(appID string, r schema.Requirement) {
	if p.emit == nil {
		return
	}
	p.mu.Lock()
	s := *p.status[key(r)]
	p.mu.Unlock()
	p.emit(appID, "app:requirement_progress", s)
}

// run executes one provisioning job end to end. Any failure lands the
// requirement in StateFailed with a message; it never panics the worker.
func (p *Provisioner) run(j job) {
	r, art := j.req, j.art
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	fail := func(err error) {
		p.log.Warn("requirement provisioning failed", "id", r.ID, "err", err)
		p.setState(r, StateFailed, -1, err.Error())
		p.emitStatus(j.appID, r)
	}

	if strings.TrimSpace(art.SHA256) == "" {
		fail(fmt.Errorf("requirement %q has no sha256 for %s — refusing to install unverified", r.ID, platformKey()))
		return
	}

	dir := p.dirFor(r)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".dl-*")
	if err != nil {
		fail(err)
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	// DOWNLOAD (throttle events to ~5% steps but keep status fresh for polling).
	p.setState(r, StateDownloading, 0, "")
	p.emitStatus(j.appID, r)
	lastPct := -1
	sha, err := download(ctx, p.hc, art.URL, tmpPath, func(done, total int64) {
		pct := 0
		if total > 0 {
			pct = int(done * 100 / total)
		}
		p.setState(r, StateDownloading, pct, "")
		if pct/5 != lastPct/5 {
			lastPct = pct
			p.emitStatus(j.appID, r)
		}
	})
	if err != nil {
		fail(err)
		return
	}

	// VERIFY sha256 — mandatory.
	p.setState(r, StateVerifying, 100, "")
	p.emitStatus(j.appID, r)
	if !strings.EqualFold(sha, strings.TrimSpace(art.SHA256)) {
		fail(fmt.Errorf("sha256 mismatch for %q: got %s want %s", r.ID, sha, art.SHA256))
		return
	}

	// INSTALL: extract / lay out, then expose on PATH via a symlink.
	p.setState(r, StateInstalling, 100, "")
	p.emitStatus(j.appID, r)
	exe, err := install(art.Format, tmpPath, dir, orDefault(art.Path, r.EffectiveBin()))
	if err != nil {
		fail(err)
		return
	}
	if err := p.link(r.EffectiveBin(), exe); err != nil {
		fail(err)
		return
	}

	// OPTIONAL smoke test.
	if c := strings.TrimSpace(r.Check); c != "" {
		if err := p.check(ctx, c); err != nil {
			fail(fmt.Errorf("post-install check failed (%s): %w", c, err))
			return
		}
	}

	if err := p.writeMarker(r, sha, exe); err != nil {
		fail(err)
		return
	}
	p.setState(r, StateReady, 100, "")
	p.emitStatus(j.appID, r)
	p.log.Info("requirement ready", "id", r.ID, "version", r.Version, "exe", exe)
}

// link exposes exe as binDir/<name>, replacing any stale entry. Symlink first,
// copy as a fallback (e.g. filesystems without symlink support).
func (p *Provisioner) link(name, exe string) error {
	dst := filepath.Join(p.binDir, name)
	_ = os.Remove(dst)
	if err := os.Symlink(exe, dst); err == nil {
		return nil
	}
	return copyFile(exe, dst, 0o755)
}

func (p *Provisioner) check(ctx context.Context, command string) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+p.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return cmd.Run()
}

func (p *Provisioner) writeMarker(r schema.Requirement, sha, exe string) error {
	b, _ := json.Marshal(marker{SHA256: sha, Exe: exe, InstalledAt: time.Now().UTC().Format(time.RFC3339)})
	return os.WriteFile(p.markerPath(r), b, 0o644)
}
