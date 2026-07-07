package codegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)


func VersionHash(def *schema.AppDefinition, cat *catalog.Catalog) (string, error) {
	h := sha256.New()
	h.Write([]byte(CompilerVersion))
	h.Write([]byte{0})
	writeSorted(h, cat.ModuleIDs())
	writeSorted(h, cat.MiddlewareNames())
	writeSorted(h, cat.ChannelTypes())
	writeSorted(h, cat.Providers())
	defJSON, err := canonicalJSON(def)
	if err != nil {
		return "", err
	}
	h.Write(defJSON)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeSorted(h interface{ Write([]byte) (int, error) }, items []string) {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	for _, s := range sorted {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	h.Write([]byte{1})
}

func canonicalJSON(v any) ([]byte, error) { return json.Marshal(v) }
