package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// safeEnvKeys is the inherited-env allow-list for a spawned stdio server.
// NODE_PATH/PYTHONPATH are excluded (library-injection); Windows process
// essentials are added so node/npx can spawn.
var safeEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LANG": true, "LC_ALL": true,
	"TERM": true, "SHELL": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"NODE_ENV": true, "XDG_DATA_HOME": true, "XDG_CONFIG_HOME": true,
	"XDG_CACHE_HOME": true, "XDG_RUNTIME_DIR": true,

	"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "PATHEXT": true,
	"COMSPEC": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"PROGRAMDATA": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true,
	"NUMBER_OF_PROCESSORS": true, "PROCESSOR_ARCHITECTURE": true,
}

// blockedEnvKeys are dropped even if the app declares them (daemon secrets).
var blockedEnvKeys = map[string]bool{
	"DIGITORN_DB_URL": true, "DIGITORN_SECRET_KEY": true, "DATABASE_URL": true,
	"DB_PASSWORD": true, "AWS_SECRET_ACCESS_KEY": true, "PRIVATE_KEY": true, "SSL_KEY": true,
}

func buildSafeEnv(declared map[string]string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		if safeEnvKeys[strings.ToUpper(kv[:i])] {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range declared {
		if blockedEnvKeys[strings.ToUpper(k)] {
			continue
		}
		env[k] = v
	}
	ensureNodeOnPath(env)
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// ensureNodeOnPath guarantees the spawned server's PATH can find `node`/`npx`.
// Most stdio MCP servers are npm packages run via npx; on machines using a node
// version manager (nvm/fnm/volta) node lives OUTSIDE the daemon's inherited PATH,
// so an npx server that works in a shell fails to spawn from the daemon with
// "executable file not found". We locate node once and prepend its bin dir.
func ensureNodeOnPath(env map[string]string) {
	dir := nodeBinDir()
	if dir == "" {
		return
	}
	path := env["PATH"]
	for _, p := range filepath.SplitList(path) {
		if strings.EqualFold(p, dir) {
			return // already present
		}
	}
	if path == "" {
		env["PATH"] = dir
	} else {
		env["PATH"] = dir + string(os.PathListSeparator) + path
	}
}

var (
	nodeBinOnce sync.Once
	nodeBinVal  string
)

// nodeBinDir resolves the directory containing the node binary, checking the
// daemon PATH first then common version-manager and standard install locations.
// Cached for the daemon's lifetime ("" when node isn't found anywhere).
func nodeBinDir() string {
	nodeBinOnce.Do(func() { nodeBinVal = findNodeBinDir() })
	return nodeBinVal
}

func findNodeBinDir() string {
	exe := "node"
	if runtime.GOOS == "windows" {
		exe = "node.exe"
	}
	if p, err := exec.LookPath(exe); err == nil {
		return filepath.Dir(p)
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".volta", "bin"),
			filepath.Join(home, "AppData", "Local", "Volta", "bin"),
			filepath.Join(home, "AppData", "Roaming", "npm"),
		)
		// nvm / fnm keep versioned installs — take the lexically-highest (latest).
		globs := []string{
			filepath.Join(home, ".nvm", "versions", "node", "*", "bin"),
			filepath.Join(home, ".local", "share", "fnm", "node-versions", "*", "installation", "bin"),
			filepath.Join(home, ".fnm", "node-versions", "*", "installation", "bin"),
			filepath.Join(home, "AppData", "Roaming", "nvm", "*"),
		}
		for _, g := range globs {
			if m, _ := filepath.Glob(g); len(m) > 0 {
				candidates = append(candidates, m[len(m)-1])
			}
		}
	}
	candidates = append(candidates, "/usr/local/bin", "/opt/homebrew/bin", `C:\Program Files\nodejs`)
	for _, dir := range candidates {
		if fi, err := os.Stat(filepath.Join(dir, exe)); err == nil && !fi.IsDir() {
			return dir
		}
	}
	return ""
}
