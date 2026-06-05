package expr

import (
	"fmt"
	"os"
)

// SecretResolver looks up a name in the secrets table, falls back to the
// environment, and otherwise passes the placeholder through so the runtime
// vault can fill it in.
func SecretResolver(table map[string]string) Resolver {
	return ResolverFunc(func(path []string) (string, error) {
		if len(path) != 1 {
			return "", fmt.Errorf("secret: expected single key, got %v", path)
		}
		if v, ok := table[path[0]]; ok {
			return v, nil
		}
		if v, ok := os.LookupEnv(path[0]); ok {
			return v, nil
		}
		return "{{secret." + path[0] + "}}", nil
	})
}
