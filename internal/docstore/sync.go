package docstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The manifest is stored inside the doc-dir itself (X.d/manifest.json), so the
// engine is self-describing on disk: the worker (agent writes) and the daemon
// (app writes) both act on it without any cross-process config plumbing.
const ManifestFile = "manifest.json"
const DirSuffix = ".d"

type SyncResult struct {
	DocDir       string       `json:"-"`
	ComposedPath string       `json:"composed"`
	Composed     bool         `json:"composed_ok"`
	Diagnostics  []Diagnostic `json:"diagnostics,omitempty"`
}

var docLocks sync.Map // absolute doc-dir → *sync.Mutex

func lockFor(dir string) *sync.Mutex {
	m, _ := docLocks.LoadOrStore(dir, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// LoadManifest reads X.d/manifest.json.
func LoadManifest(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

// WriteManifest seeds X.d/manifest.json (used at bind/explode time).
func WriteManifest(dir string, m Manifest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o644)
}

// FindDocDir classifies an absolute path against the fragment layout.
// kind is "fragment" (path lives under a doc-dir), "composed" (path IS a
// composed document with a sibling doc-dir), or "" (not doc-store related).
func FindDocDir(abs string) (dir, kind string) {
	if d := abs + DirSuffix; hasManifest(d) {
		return d, "composed"
	}
	for cur := filepath.Dir(abs); ; cur = filepath.Dir(cur) {
		if strings.HasSuffix(cur, DirSuffix) && hasManifest(cur) {
			return cur, "fragment"
		}
		if parent := filepath.Dir(cur); parent == cur {
			return "", ""
		}
	}
}

func hasManifest(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ManifestFile))
	return err == nil && !fi.IsDir()
}

// ComposedPath maps a doc-dir back to its composed file (strip the suffix).
func ComposedPath(docDir string) string { return strings.TrimSuffix(docDir, DirSuffix) }

// SyncFragments validates + composes a doc-dir after a fragment mutation.
// On success the composed file is atomically (re)written and the journal
// updated; on error-severity diagnostics the composed file is left untouched.
func SyncFragments(docDir string) (SyncResult, error) {
	mu := lockFor(docDir)
	mu.Lock()
	defer mu.Unlock()

	res := SyncResult{DocDir: docDir, ComposedPath: ComposedPath(docDir)}
	m, err := LoadManifest(docDir)
	if err != nil {
		return res, err
	}
	composed, diags, err := Compose(m, docDir)
	res.Diagnostics = diags
	if err != nil {
		return res, err // ErrInvalid: diagnostics carry the story, composed untouched
	}
	tmp := res.ComposedPath + ".tmp"
	if werr := os.WriteFile(tmp, composed, 0o644); werr != nil {
		return res, werr
	}
	if werr := os.Rename(tmp, res.ComposedPath); werr != nil {
		return res, werr
	}
	j := LoadJournal(docDir)
	RecordComposed(j, composed, m, docDir)
	if serr := j.Save(docDir); serr != nil {
		return res, serr
	}
	_ = GenerateOverview(m, docDir)
	res.Composed = true
	return res, nil
}

// SyncComposed decomposes an app-written composed file back onto fragments.
// Returns the touched fragment paths (relative to the doc-dir).
func SyncComposed(composedPath string) (changed []string, err error) {
	docDir := composedPath + DirSuffix
	mu := lockFor(docDir)
	mu.Lock()
	defer mu.Unlock()

	m, err := LoadManifest(docDir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(composedPath)
	if err != nil {
		return nil, err
	}
	j := LoadJournal(docDir)
	changed, err = Decompose(m, b, docDir, j)
	if err != nil {
		return nil, err
	}
	if serr := j.Save(docDir); serr != nil {
		return changed, serr
	}
	if len(changed) > 0 {
		_ = GenerateOverview(m, docDir)
	}
	return changed, nil
}

// ExplodeFile converts an existing monolithic document into a doc-dir seeded
// with the given manifest, then records the journal. The original file stays
// in place as the composed artifact.
func ExplodeFile(composedPath string, m Manifest) error {
	docDir := composedPath + DirSuffix
	mu := lockFor(docDir)
	mu.Lock()
	defer mu.Unlock()

	b, err := os.ReadFile(composedPath)
	if err != nil {
		return err
	}
	if err := WriteManifest(docDir, m); err != nil {
		return err
	}
	j, err := Explode(m, b, docDir)
	if err != nil {
		return err
	}
	j.ComposedHash = hashRaw(b)
	if serr := j.Save(docDir); serr != nil {
		return serr
	}
	return GenerateOverview(m, docDir)
}
