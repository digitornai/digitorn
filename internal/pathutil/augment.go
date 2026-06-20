// Package pathutil augments the process PATH with well-known tool directories
// so exec.LookPath and every spawned subprocess (LSP servers, MCP sidecars,
// bash commands) resolve developer tools regardless of how the daemon was
// launched (thin shell, systemd service, launchd, Windows SCM, IDE).
//
// Call AugmentPath() once, as early as possible in main(), before any module
// or server bootstrap runs.
package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AugmentPath prepends well-known tool directories that exist on disk to the
// process PATH. It is idempotent and safe to call multiple times. Directories
// already in PATH are never duplicated.
func AugmentPath() {
	home, _ := os.UserHomeDir()
	candidates := wellKnownDirs(home)

	sep := string(filepath.ListSeparator)
	current := os.Getenv("PATH")

	existing := make(map[string]bool, 32)
	for _, d := range filepath.SplitList(current) {
		existing[filepath.Clean(d)] = true
	}

	var extra []string
	for _, d := range candidates {
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if existing[d] {
			continue
		}
		if _, err := os.Stat(d); err == nil {
			extra = append(extra, d)
			existing[d] = true // prevent dups within candidates
		}
	}
	if len(extra) == 0 {
		return
	}
	_ = os.Setenv("PATH", strings.Join(extra, sep)+sep+current)
}

// wellKnownDirs returns the candidate directories to prepend, in priority
// order (highest priority first — these get prepended, so earlier = wins).
func wellKnownDirs(home string) []string {
	var dirs []string

	add := func(d string) { dirs = append(dirs, d) }

	// ── Go ──────────────────────────────────────────────────────────────────
	// $GOPATH/bin when GOPATH is explicitly set; fall back to ~/go/bin.
	if gp := os.Getenv("GOPATH"); gp != "" {
		add(filepath.Join(gp, "bin"))
	}
	if home != "" {
		add(filepath.Join(home, "go", "bin"))
	}
	// Common system-wide Go installation (Linux packages, official installer).
	add("/usr/local/go/bin")

	// ── Node / npm / nvm ────────────────────────────────────────────────────
	// $NVM_BIN is set by nvm when a shell sources it — respect it when present.
	if nb := os.Getenv("NVM_BIN"); nb != "" {
		add(nb)
	}
	// Discover all nvm-managed node versions and add their bin dirs.
	// This works even when the daemon was launched without nvm sourced.
	if home != "" {
		nvmDir := os.Getenv("NVM_DIR")
		if nvmDir == "" {
			nvmDir = filepath.Join(home, ".nvm")
		}
		// ~/.nvm/versions/node/v*/bin — add newest first (Glob is sorted).
		pattern := filepath.Join(nvmDir, "versions", "node", "*", "bin")
		if matches, err := filepath.Glob(pattern); err == nil {
			// Reverse so the highest version (lexically last) is tried first.
			for i := len(matches) - 1; i >= 0; i-- {
				add(matches[i])
			}
		}
	}
	// Common non-nvm global npm prefix locations.
	if home != "" {
		add(filepath.Join(home, ".npm-global", "bin"))
		add(filepath.Join(home, ".npm", "bin"))
	}

	// ── Python ──────────────────────────────────────────────────────────────
	if home != "" {
		// pip install --user puts scripts here on Linux.
		add(filepath.Join(home, ".local", "bin"))
		// pyenv shims + bin.
		pyenvRoot := os.Getenv("PYENV_ROOT")
		if pyenvRoot == "" {
			pyenvRoot = filepath.Join(home, ".pyenv")
		}
		add(filepath.Join(pyenvRoot, "shims"))
		add(filepath.Join(pyenvRoot, "bin"))
	}

	// ── Rust / Cargo ─────────────────────────────────────────────────────────
	if home != "" {
		cargoHome := os.Getenv("CARGO_HOME")
		if cargoHome == "" {
			cargoHome = filepath.Join(home, ".cargo")
		}
		add(filepath.Join(cargoHome, "bin"))
	}

	// ── Ruby / rbenv ─────────────────────────────────────────────────────────
	if home != "" {
		rbenvRoot := os.Getenv("RBENV_ROOT")
		if rbenvRoot == "" {
			rbenvRoot = filepath.Join(home, ".rbenv")
		}
		add(filepath.Join(rbenvRoot, "shims"))
		add(filepath.Join(rbenvRoot, "bin"))
	}

	// ── macOS Homebrew ───────────────────────────────────────────────────────
	if runtime.GOOS == "darwin" {
		// Apple Silicon default prefix.
		add("/opt/homebrew/bin")
		add("/opt/homebrew/sbin")
		// Intel / legacy prefix (usually already in PATH, but cheap to add).
		add("/usr/local/bin")
		add("/usr/local/sbin")
	}

	return dirs
}
