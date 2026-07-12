package docstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Explode splits a composed document into fragments under dir and seeds the
// sync journal. Fragments are written pretty-printed (they are meant to be
// read and edited by the agent).
func Explode(m Manifest, composed []byte, dir string) (*Journal, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(composed, &top); err != nil {
		return nil, fmt.Errorf("composed document is not a JSON object: %w", err)
	}
	j := &Journal{Items: map[string]string{}, Files: map[string]string{}}

	for _, c := range m.Collections {
		key, err := c.pointerKey()
		if err != nil {
			return nil, err
		}
		raw, ok := top[key]
		delete(top, key)
		base := filepath.Join(dir, filepath.FromSlash(c.Name))
		if err := os.MkdirAll(base, 0o755); err != nil {
			return nil, err
		}
		if !ok || len(raw) == 0 {
			continue
		}
		items, err := splitItems(c, raw)
		if err != nil {
			return nil, fmt.Errorf("collection %s: %w", c.Name, err)
		}
		for _, it := range items {
			name := sanitizeID(it.id) + ".json"
			if err := writePretty(filepath.Join(base, name), it.raw); err != nil {
				return nil, err
			}
			j.Items[c.Name+"/"+it.id] = hashRaw(it.raw)
			j.Files[c.Name+"/"+it.id] = name
		}
	}

	rootFile := m.Root
	if rootFile == "" {
		rootFile = "meta.json"
	}
	rootRaw, err := json.Marshal(top)
	if err != nil {
		return nil, err
	}
	if err := writePretty(filepath.Join(dir, rootFile), rootRaw); err != nil {
		return nil, err
	}
	j.Items["/root"] = hashRaw(rootRaw)
	j.ComposedHash = hashRaw(composed)
	return j, nil
}

type item struct {
	id  string
	raw json.RawMessage
}

func splitItems(c Collection, raw json.RawMessage) ([]item, error) {
	if c.isMap() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out := make([]item, 0, len(m))
		for k, v := range m {
			out = append(out, item{id: k, raw: v})
		}
		return out, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, err
	}
	out := make([]item, 0, len(arr))
	for i, v := range arr {
		var obj map[string]any
		if err := json.Unmarshal(v, &obj); err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		id, _ := obj[c.ID].(string)
		if id == "" {
			id = generatedID(v, i)
			obj[c.ID] = id
			nv, err := json.Marshal(obj)
			if err != nil {
				return nil, err
			}
			v = nv
		}
		out = append(out, item{id: id, raw: v})
	}
	return out, nil
}

func generatedID(raw json.RawMessage, i int) string {
	return fmt.Sprintf("gen_%s_%d", hashRaw(raw)[:6], i)
}

func writePretty(path string, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		buf.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
