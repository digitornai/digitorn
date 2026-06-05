package bash

import (
	"os"
	"runtime"
	"strings"
)

var nonInteractiveEnv = map[string]string{
	"CI":                            "true",
	"CONTINUOUS_INTEGRATION":        "true",
	"DEBIAN_FRONTEND":               "noninteractive",
	"DEBCONF_NONINTERACTIVE_SEEN":   "true",
	"EDITOR":                        "true",
	"VISUAL":                        "true",
	"GIT_EDITOR":                    "true",
	"PAGER":                         "cat",
	"GIT_PAGER":                     "cat",
	"MANPAGER":                      "cat",
	"npm_config_yes":                "true",
	"npm_config_audit":              "false",
	"npm_config_fund":               "false",
	"npm_config_progress":           "false",
	"npm_config_loglevel":           "warn",
	"YARN_ENABLE_TELEMETRY":         "0",
	"GIT_TERMINAL_PROMPT":           "0",
	"GIT_ASKPASS":                   "true",
	"SSH_ASKPASS":                   "true",
	"NO_COLOR":                      "1",
	"FORCE_COLOR":                   "0",
	"CLICOLOR":                      "0",
	"CLICOLOR_FORCE":                "0",
	"PYTHONUNBUFFERED":              "1",
	"PYTHONDONTWRITEBYTECODE":       "1",
	"PIP_DISABLE_PIP_VERSION_CHECK": "1",
	"PIP_NO_INPUT":                  "1",
	"HOMEBREW_NO_AUTO_UPDATE":       "1",
	"HOMEBREW_NO_INSTALL_CLEANUP":   "1",
	"HOMEBREW_NO_ENV_HINTS":         "1",
	"AWS_PAGER":                     "",
	"CARGO_TERM_PROGRESS_WHEN":      "never",
	"CARGO_TERM_COLOR":              "never",
	"PGCONNECT_TIMEOUT":             "10",
}

func baseAllow() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"SystemRoot", "windir", "COMSPEC", "PATHEXT", "PATH", "TEMP", "TMP",
			"HOMEDRIVE", "HOMEPATH", "USERPROFILE", "USERNAME", "APPDATA",
			"LOCALAPPDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432",
			"PROGRAMDATA", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
			"PSMODULEPATH",
		}
	}
	return []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
		"TERM", "TMPDIR", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR",
	}
}

func buildEnv(extraAllow []string) []string {
	ci := runtime.GOOS == "windows"
	allow := map[string]bool{}
	add := func(k string) {
		if ci {
			k = strings.ToUpper(k)
		}
		allow[k] = true
	}
	for _, k := range baseAllow() {
		add(k)
	}
	for _, k := range extraAllow {
		add(k)
	}
	out := map[string]string{}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		key := k
		if ci {
			key = strings.ToUpper(k)
		}
		if allow[key] {
			out[k] = v
		}
	}
	for k, v := range nonInteractiveEnv {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	// Make tool resolution independent of how the daemon was launched : merge the
	// persisted PATH (Windows registry user+machine) so `python`/`node`/etc.
	// resolve even when the inherited process PATH is thin. No-op off Windows.
	for k, v := range out {
		if strings.EqualFold(k, "path") {
			out[k] = enrichPath(v)
			break
		}
	}
	env := make([]string, 0, len(out))
	for k, v := range out {
		env = append(env, k+"="+v)
	}
	return env
}
