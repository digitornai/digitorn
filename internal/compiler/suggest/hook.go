package suggest

import "github.com/digitornai/digitorn/internal/compiler/parse"

func init() {
	parse.SetClosestMatchHook(func(target string, pool []string) (string, bool) {
		return Closest(target, pool, 2)
	})
}
