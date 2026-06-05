package goshell

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// busybox.exe is a single self-contained native Win32 executable (no DLL, no
// install, fully relocatable — unlike Git's MSYS bash). It supplies the GNU
// coreutils the agent's pipelines need (sed, awk, grep, sort, cut, tr, wc,
// xargs, find, date, …) on a Windows host that has no real bash. GPLv2 — see
// the NOTICE file in this directory.
//
//go:embed busybox.exe
var busyboxBin []byte

var (
	bbOnce sync.Once
	bbDir  string
)

var appletName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// busyboxDir extracts busybox once per process into a versioned cache dir,
// hard-links every applet as <name>.exe next to it, and returns that dir. The
// caller prepends it to PATH so the agent's `sed`/`awk`/`xargs` — and busybox's
// own sub-execs (xargs→wc, find→…) — all resolve. "" if extraction fails.
func busyboxDir() string {
	bbOnce.Do(extractBusybox)
	return bbDir
}

func extractBusybox() {
	sum := sha256.Sum256(busyboxBin)
	ver := hex.EncodeToString(sum[:6])

	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "digitorn", "busybox", ver)
	bb := filepath.Join(dir, "busybox.exe")
	ready := filepath.Join(dir, ".ready") // written last; its presence means links are done

	if _, e := os.Stat(ready); e == nil {
		bbDir = dir
		return
	}
	if e := os.MkdirAll(dir, 0o755); e != nil {
		return
	}
	if fi, e := os.Stat(bb); e != nil || fi.Size() != int64(len(busyboxBin)) {
		tmp := filepath.Join(dir, fmt.Sprintf(".bb-%d.tmp", os.Getpid()))
		if e := os.WriteFile(tmp, busyboxBin, 0o755); e != nil {
			return
		}
		if e := os.Rename(tmp, bb); e != nil {
			_ = os.Remove(tmp)
			if fi, se := os.Stat(bb); se != nil || fi.Size() != int64(len(busyboxBin)) {
				return
			}
		}
	}

	for _, name := range listApplets(bb) {
		if name == "busybox" || !appletName.MatchString(name) {
			continue
		}
		link := filepath.Join(dir, name+".exe")
		if _, e := os.Stat(link); e == nil {
			continue
		}
		if os.Link(bb, link) != nil { // hard-link (free on NTFS); fall back to copy
			_ = os.WriteFile(link, busyboxBin, 0o755)
		}
	}
	_ = os.WriteFile(ready, nil, 0o644)
	bbDir = dir
}

func listApplets(p string) []string {
	out, err := exec.Command(p, "--list").Output()
	if err != nil {
		return nil
	}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if n := strings.TrimSpace(sc.Text()); n != "" {
			names = append(names, n)
		}
	}
	return names
}
