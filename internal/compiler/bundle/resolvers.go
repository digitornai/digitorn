package bundle

import (
	"fmt"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/expr"
)

func PromptResolver(b *Bundle) expr.Resolver {
	return expr.ResolverFunc(func(path []string) (string, error) {
		if len(path) != 1 {
			return "", fmt.Errorf("prompt: expected single key, got %v", path)
		}
		return b.ReadPrompt(path[0])
	})
}

func SkillResolver(b *Bundle) expr.Resolver {
	return expr.ResolverFunc(func(path []string) (string, error) {
		if len(path) != 1 {
			return "", fmt.Errorf("skill: expected single key, got %v", path)
		}
		return b.ReadSkill(path[0])
	})
}

func BehaviorResolver(b *Bundle) expr.Resolver {
	return expr.ResolverFunc(func(path []string) (string, error) {
		if len(path) != 1 {
			return "", fmt.Errorf("behavior: expected single key, got %v", path)
		}
		return b.ReadBehavior(path[0])
	})
}

// AssetResolver joins all path segments back with '.' so {{asset.logo.png}}
// targets the file `logo.png`.
func AssetResolver(b *Bundle, appID string) expr.Resolver {
	return expr.ResolverFunc(func(path []string) (string, error) {
		if len(path) == 0 {
			return "", fmt.Errorf("asset: expected file name")
		}
		return b.AssetURL(appID, strings.Join(path, "."))
	})
}

func AssetBase64Resolver(b *Bundle) expr.Resolver {
	return expr.ResolverFunc(func(path []string) (string, error) {
		if len(path) == 0 {
			return "", fmt.Errorf("asset_b64: expected file name")
		}
		return b.AssetBase64(strings.Join(path, "."))
	})
}

type includeBridge struct{ b *Bundle }

func (i *includeBridge) ResolveInclude(path string) (string, error) {
	return i.b.ReadInclude(path)
}

func IncludeResolver(b *Bundle) expr.IncludeResolver {
	return &includeBridge{b: b}
}
