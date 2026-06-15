package sessionstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const snapshotVersion = 1

const defaultBinarySnapshotThreshold = 5 << 20

type SessionSnapshot struct {
	Version               int                       `json:"version"`
	SessionID             string                    `json:"session_id"`
	AppID                 string                    `json:"app_id,omitempty"`
	UserID                string                    `json:"user_id,omitempty"`
	StartedAtNano         int64                     `json:"started_at"`
	EndedAtNano           int64                     `json:"ended_at,omitempty"`
	CapturedAtNano        int64                     `json:"captured_at"`
	FirstSeq              uint64                    `json:"first_seq"`
	LastSeq               uint64                    `json:"last_seq"`
	CutoffSeq             uint64                    `json:"cutoff_seq"`
	Messages              []Message                 `json:"messages"`
	ToolCalls             map[string]*ToolCallState `json:"tool_calls"`
	Approvals             map[string]*ApprovalState `json:"approvals"`
	Memory                map[string]string         `json:"memory"`
	Facts                 []string                  `json:"facts,omitempty"`
	Goal                  string                    `json:"goal,omitempty"`
	WorkspaceFiles        map[string]*FileState     `json:"workspace_files"`
	Todos                 []Todo                    `json:"todos,omitempty"`
	Children              []ChildAgent              `json:"children,omitempty"`
	BackgroundTasks       []BackgroundTaskState     `json:"background_tasks,omitempty"`
	Widgets               map[string]*WidgetState   `json:"widgets,omitempty"`
	Previews              map[string]*PreviewState  `json:"previews,omitempty"`
	Errors                []ErrorEntry              `json:"errors,omitempty"`
	Compactions           []CompactionEntry         `json:"compactions,omitempty"`
	ContextCompaction     *ContextCompactionState   `json:"context_compaction,omitempty"`
	PreparedSummary       *PreparedSummaryState     `json:"prepared_summary,omitempty"`
	CompactionInflight    bool                      `json:"compaction_inflight,omitempty"`
	ContextTokens         int                       `json:"context_tokens,omitempty"`
	ContextSystemTokens   int                       `json:"context_system_tokens,omitempty"`
	ContextToolsTokens    int                       `json:"context_tools_tokens,omitempty"`
	ContextMessageTokens  int                       `json:"context_message_tokens,omitempty"`
	ContextProviderTokens int                       `json:"context_provider_tokens,omitempty"`
	TokensIn              int64                     `json:"tokens_in,omitempty"`
	TokensOut             int64                     `json:"tokens_out,omitempty"`
	UsdTotal              float64                   `json:"usd_total,omitempty"`
	Title                 string                    `json:"title,omitempty"`
	Workspace             string                    `json:"workspace,omitempty"`
	Workdir               string                    `json:"workdir,omitempty"`
	EntryAgent            string                    `json:"entry_agent,omitempty"`
	ContextExtra          string                    `json:"context,omitempty"`
	ModelOverrides        map[string]string         `json:"model_overrides,omitempty"`
	TurnCount             int                       `json:"turn_count,omitempty"`
	Interrupted           bool                      `json:"interrupted,omitempty"`

	// CurrentTurn* mirrors the SessionState fields ; populated when a
	// turn is in flight, cleared when EventTurnEnded fires. Used by
	// recovery to detect mid-flight turns at boot.
	CurrentTurnID            string `json:"current_turn_id,omitempty"`
	CurrentTurnPhase         string `json:"current_turn_phase,omitempty"`
	CurrentTurnStartedAtNano int64  `json:"current_turn_started_at,omitempty"`
	ActiveMode               string `json:"active_mode,omitempty"`

	Closed     bool   `json:"closed,omitempty"`
	EventCount uint64 `json:"event_count"`
	BytesEst   int64  `json:"bytes_est,omitempty"`
	Partial    bool   `json:"partial,omitempty"`
}

type SnapshotFormat int

const (
	SnapshotJSON SnapshotFormat = iota
	SnapshotBinary
)

type WriteSnapshotResult struct {
	Path      string
	Format    SnapshotFormat
	Bytes     int
	SHA256    string
	WrittenAt int64
}

func WriteSnapshotAtomic(dir string, snap SessionSnapshot, format SnapshotFormat, fsync bool) (*WriteSnapshotResult, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot mkdir: %w", err)
	}
	var (
		data   []byte
		target string
		alt    string
		encErr error
	)
	switch format {
	case SnapshotJSON:
		data, encErr = json.Marshal(snap)
		target = filepath.Join(dir, snapshotFilename)
		alt = filepath.Join(dir, snapshotBinFilename)
	case SnapshotBinary:
		buf, err := gobEncode(snap)
		if err != nil {
			return nil, err
		}
		data = buf
		target = filepath.Join(dir, snapshotBinFilename)
		alt = filepath.Join(dir, snapshotFilename)
	default:
		return nil, fmt.Errorf("snapshot: unknown format %d", format)
	}
	if encErr != nil {
		return nil, fmt.Errorf("snapshot encode: %w", encErr)
	}

	tmp, err := os.CreateTemp(dir, tmpSnapshotPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("snapshot tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("snapshot write: %w", err)
	}
	if fsync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return nil, fmt.Errorf("snapshot fsync: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("snapshot close: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return nil, fmt.Errorf("snapshot rename: %w", err)
	}
	cleanup = false

	// Drop the alternate-format file if it exists — a session has a single
	// canonical snapshot. Failure to remove is non-fatal.
	if _, err := os.Stat(alt); err == nil {
		_ = os.Remove(alt)
	}

	h := sha256.Sum256(data)
	res := &WriteSnapshotResult{
		Path:   target,
		Format: format,
		Bytes:  len(data),
		SHA256: hex.EncodeToString(h[:]),
	}
	return res, nil
}

func ReadSnapshot(dir string) (*SessionSnapshot, SnapshotFormat, error) {
	jsonPath := filepath.Join(dir, snapshotFilename)
	binPath := filepath.Join(dir, snapshotBinFilename)

	if data, err := os.ReadFile(binPath); err == nil {
		var snap SessionSnapshot
		if err := gobDecode(data, &snap); err != nil {
			return nil, SnapshotBinary, fmt.Errorf("snapshot bin decode: %w", err)
		}
		return &snap, SnapshotBinary, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, SnapshotBinary, fmt.Errorf("snapshot bin read: %w", err)
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, SnapshotJSON, nil
		}
		return nil, SnapshotJSON, fmt.Errorf("snapshot read: %w", err)
	}
	if len(data) == 0 {
		return nil, SnapshotJSON, nil
	}
	var snap SessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, SnapshotJSON, fmt.Errorf("snapshot decode: %w", err)
	}
	return &snap, SnapshotJSON, nil
}

func VerifySnapshot(data []byte, expectedHex string) error {
	if expectedHex == "" {
		return nil
	}
	h := sha256.Sum256(data)
	got := hex.EncodeToString(h[:])
	if got != expectedHex {
		return fmt.Errorf("snapshot integrity mismatch: want %s got %s", expectedHex, got)
	}
	return nil
}

func gobEncode(snap SessionSnapshot) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(snap); err != nil {
		return nil, fmt.Errorf("snapshot gob encode: %w", err)
	}
	return buf.Bytes(), nil
}

func gobDecode(data []byte, snap *SessionSnapshot) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(snap)
}
