package expr

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

// EnvResolver looks up OS environment variables. Missing names produce a
// passthrough (the runtime may resolve them later) rather than an error,
// matching the Python compiler's lenient behaviour. Use StrictEnvResolver
// in CI / deployment to catch missing env vars at compile time.
//
// An env var that is SET BUT EMPTY ("ANTHROPIC_API_KEY=") is treated the
// same as unset : the placeholder is preserved. This avoids a footgun
// where a shell session that previously exported the secret then cleared
// it (`$env:X = ""` on PowerShell) silently produces a literal empty
// string in the bundle, which downstream validators (hasAuth, etc.)
// reject as "no credential present".
func EnvResolver() Resolver {
	return ResolverFunc(func(path []string) (string, error) {
		if len(path) != 1 {
			return "", fmt.Errorf("env: expected single key, got %v", path)
		}
		if v, ok := os.LookupEnv(path[0]); ok && v != "" {
			return v, nil
		}
		if path[0] == "PWD" {
			if wd, err := os.Getwd(); err == nil {
				return wd, nil
			}
		}
		return "{{env." + path[0] + "}}", nil
	})
}

// StrictEnvResolver is the strict counterpart of EnvResolver. It returns
// an "unresolved env" error when the variable is not in the process
// environment, which becomes a DGT-E0201 (CodeMissingEnvVar) diagnostic
// at compile time. Use this in CI pipelines or release builds where a
// missing env var must fail the build rather than silently pass through.
func StrictEnvResolver() Resolver {
	return ResolverFunc(func(path []string) (string, error) {
		if len(path) != 1 {
			return "", fmt.Errorf("env: expected single key, got %v", path)
		}
		if v, ok := os.LookupEnv(path[0]); ok {
			return v, nil
		}
		if path[0] == "PWD" {
			if wd, err := os.Getwd(); err == nil {
				return wd, nil
			}
		}
		// "unresolved" is the magic substring that the YAML walker's
		// codeFor() function maps to CodeMissingEnvVar (DGT-E0201).
		return "", fmt.Errorf("env.%s: unresolved (set the variable or compile with Strict=false)", path[0])
	})
}

func SysResolver() Resolver {
	return ResolverFunc(func(path []string) (string, error) {
		if len(path) != 1 {
			return "", fmt.Errorf("sys: expected single key, got %v", path)
		}
		switch path[0] {
		case "timestamp":
			return time.Now().UTC().Format(time.RFC3339), nil
		case "date":
			return time.Now().UTC().Format("2006-01-02"), nil
		case "time":
			return time.Now().UTC().Format("15:04:05"), nil
		case "hostname":
			h, _ := os.Hostname()
			return h, nil
		case "platform", "os":
			return runtime.GOOS, nil
		case "arch":
			return runtime.GOARCH, nil
		case "cwd":
			wd, _ := os.Getwd()
			return wd, nil
		case "user":
			if v, ok := os.LookupEnv("USER"); ok {
				return v, nil
			}
			if v, ok := os.LookupEnv("USERNAME"); ok {
				return v, nil
			}
			return "", nil
		case "pid":
			return fmt.Sprintf("%d", os.Getpid()), nil
		case "home":
			h, _ := os.UserHomeDir()
			return h, nil
		case "tmpdir", "temp_dir":
			return os.TempDir(), nil
		case "shell":
			if v, ok := os.LookupEnv("SHELL"); ok {
				return v, nil
			}
			if v, ok := os.LookupEnv("ComSpec"); ok {
				return v, nil
			}
			return "", nil
		case "is_windows":
			return boolStr(runtime.GOOS == "windows"), nil
		case "is_linux":
			return boolStr(runtime.GOOS == "linux"), nil
		case "is_macos":
			return boolStr(runtime.GOOS == "darwin"), nil
		case "path_sep":
			if runtime.GOOS == "windows" {
				return "\\", nil
			}
			return "/", nil
		case "locale":
			return os.Getenv("LANG"), nil
		case "shell_family":
			return shellFamily(), nil
		default:
			return "{{sys." + path[0] + "}}", nil
		}
	})
}

// MapResolver answers from a flat string map and passes the placeholder
// through when the key is absent (lenient by design).
func MapResolver(values map[string]string) Resolver {
	return ResolverFunc(func(path []string) (string, error) {
		key := path[0]
		if len(path) > 1 {
			for _, p := range path[1:] {
				key += "." + p
			}
		}
		if v, ok := values[key]; ok {
			return v, nil
		}
		return "", ErrUnresolved
	})
}

// LenientMapResolver behaves like MapResolver but returns a passthrough
// placeholder when the key is absent, so the runtime can fill it in.
func LenientMapResolver(namespace string, values map[string]string) Resolver {
	return ResolverFunc(func(path []string) (string, error) {
		key := path[0]
		if len(path) > 1 {
			for _, p := range path[1:] {
				key += "." + p
			}
		}
		if v, ok := values[key]; ok {
			return v, nil
		}
		return "{{" + namespace + "." + key + "}}", nil
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func shellFamily() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "bash"
}
