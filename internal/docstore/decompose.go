package docstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func Decompose(m Manifest, composed []byte, dir string, j *Journal) (changed []string, err error) {
	if j.ComposedHash != "" && hashRaw(composed) == j.ComposedHash {
		return nil, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(composed, &top); err != nil {
		return nil, fmt.Errorf("composed document is not a JSON object: %w", err)
	}

	for _, c := range m.Collections {
		key, perr := c.pointerKey()
		if perr != nil {
			return nil, perr
		}
		raw := top[key]
		delete(top, key)
		items := []item{}
		if len(raw) > 0 {
			items, err = splitItems(c, raw)
			if err != nil {
				return nil, fmt.Errorf("collection %s: %w", c.Name, err)
			}
		}
		base := filepath.Join(dir, filepath.FromSlash(c.Name))
		if err := os.MkdirAll(base, 0o755); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, it := range items {
			if isGeneratedID(m, it.id) {
				continue
			}
			jk := c.Name + "/" + it.id
			if name := j.Files[jk]; name != "" {
				if old, rerr := os.ReadFile(filepath.Join(base, name)); rerr == nil {
					it.raw = preserveAuthored(m, old, it.raw)
				}
			}
			it.raw = stripDerived(m, it.raw)
			seen[jk] = true
			h := hashRaw(it.raw)
			if j.Items[jk] == h {
				continue
			}
			name := j.Files[jk]
			if name == "" {
				name = sanitizeID(it.id) + ".json"
			}
			if err := writePretty(filepath.Join(base, name), it.raw); err != nil {
				return nil, err
			}
			j.Items[jk] = h
			j.Files[jk] = name
			changed = append(changed, c.Name+"/"+name)
		}
		for jk, name := range j.Files {
			if len(jk) > len(c.Name) && jk[:len(c.Name)+1] == c.Name+"/" && !seen[jk] {
				_ = os.Remove(filepath.Join(base, name))
				delete(j.Items, jk)
				delete(j.Files, jk)
				changed = append(changed, c.Name+"/"+name)
			}
		}
	}

	rootRaw, err := json.Marshal(top)
	if err != nil {
		return nil, err
	}
	if h := hashRaw(rootRaw); j.Items["/root"] != h {
		rootFile := m.Root
		if rootFile == "" {
			rootFile = "meta.json"
		}
		if err := writePretty(filepath.Join(dir, rootFile), rootRaw); err != nil {
			return nil, err
		}
		j.Items["/root"] = h
		changed = append(changed, rootFile)
	}

	j.ComposedHash = hashRaw(composed)
	return changed, nil
}

func RecordComposed(j *Journal, composed []byte, m Manifest, dir string) {
	j.ComposedHash = hashRaw(composed)
	for _, c := range m.Collections {
		base := filepath.Join(dir, filepath.FromSlash(c.Name))
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(base, e.Name()))
			if err != nil {
				continue
			}
			var obj map[string]any
			if json.Unmarshal(b, &obj) != nil {
				continue
			}
			id := ""
			if c.isMap() {
				id = e.Name()[:len(e.Name())-len(".json")]
			} else {
				id, _ = obj[c.ID].(string)
			}
			if id == "" {
				continue
			}
			j.Items[c.Name+"/"+id] = hashRaw(b)
			j.Files[c.Name+"/"+id] = e.Name()
		}
	}
}
