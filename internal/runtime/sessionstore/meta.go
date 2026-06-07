package sessionstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Meta struct {
	SessionID      string `json:"session_id"`
	AppID          string `json:"app_id,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	FirstSeq       uint64 `json:"first_seq"`
	LastSeq        uint64 `json:"last_seq"`
	EventCount     uint64 `json:"event_count"`
	StartedAtNano  int64  `json:"started_at"`
	UpdatedAtNano  int64  `json:"updated_at"`
	SnapshotCutoff uint64 `json:"snapshot_cutoff,omitempty"`
	SnapshotSHA256 string `json:"snapshot_sha256,omitempty"`
	SnapshotBinary bool   `json:"snapshot_binary,omitempty"`
	Partial        bool   `json:"partial,omitempty"`
	Title          string `json:"title,omitempty"`
	Workspace      string `json:"workspace,omitempty"`
	Workdir        string `json:"workdir,omitempty"`
	EntryAgent     string `json:"entry_agent,omitempty"`
	ContextExtra   string `json:"context,omitempty"`
	// Preview is a short snippet of the session's first user message, cached here
	// so the list endpoint can label sessions by topic without reading each
	// session's events file (the scaling bottleneck at thousands of sessions).
	Preview string `json:"preview,omitempty"`
}

// CapPreview collapses whitespace and caps a preview snippet to a single short
// line. Shared so meta-written previews and list-endpoint previews stay
// byte-identical.
func CapPreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

func ReadMeta(path string) (*Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("meta read: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("meta decode: %w", err)
	}
	return &m, nil
}

func WriteMetaAtomic(dir string, m *Meta, fsync bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("meta mkdir: %w", err)
	}
	if m.UpdatedAtNano == 0 {
		m.UpdatedAtNano = time.Now().UnixNano()
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("meta encode: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tmpMetaPrefix+"*")
	if err != nil {
		return fmt.Errorf("meta tmp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("meta write: %w", err)
	}
	if fsync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return fmt.Errorf("meta sync: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("meta close: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, metaFilename)); err != nil {
		return fmt.Errorf("meta rename: %w", err)
	}
	return nil
}
