package docstore

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Journal is the per-document sync state: content hashes for echo suppression
// (anti-loop) and per-item hashes so decompose only rewrites what changed.
type Journal struct {
	ComposedHash string            `json:"composed_hash"`
	Items        map[string]string `json:"items"` // "<collection>/<id>" and "/root" → hash
	Files        map[string]string `json:"files"` // "<collection>/<id>" → fragment basename
}

const journalDir = "_index"
const journalFile = ".sync.json"

func journalPath(dir string) string { return filepath.Join(dir, journalDir, journalFile) }

func LoadJournal(dir string) *Journal {
	j := &Journal{Items: map[string]string{}, Files: map[string]string{}}
	b, err := os.ReadFile(journalPath(dir))
	if err != nil {
		return j
	}
	_ = json.Unmarshal(b, j)
	if j.Items == nil {
		j.Items = map[string]string{}
	}
	if j.Files == nil {
		j.Files = map[string]string{}
	}
	return j
}

func (j *Journal) Save(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, journalDir), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := journalPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, journalPath(dir))
}
