package mcp

import (
	"os"
	"strings"
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
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
