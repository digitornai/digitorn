package tool

import (
	"context"
	"encoding/json"
)

type Handler func(ctx context.Context, params json.RawMessage) (Result, error)

type Result struct {
	Success  bool           `json:"success"`
	Data     any            `json:"data,omitempty"`
	Error    string         `json:"error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Display  *DisplayHint   `json:"display,omitempty"`
	Diff *DiffView `json:"-"`

	OutputParts []OutputPart `json:"-"`
}

const (
	OutputText  = "text"
	OutputImage = "image"
	OutputAudio = "audio"
	OutputVideo = "video"
	OutputFile  = "file"
)

type OutputPart struct {
	Kind  string `json:"kind"`
	Text  string `json:"text,omitempty"`
	Bytes []byte `json:"-"`
	Mime  string `json:"mime,omitempty"`
	Name  string `json:"name,omitempty"`
}

type DiffView struct {
	Unified         string `json:"unified_diff,omitempty"`
	Summary         string `json:"diff,omitempty"`
	PreviousContent string `json:"previous_content,omitempty"`
	NewContent      string `json:"new_content,omitempty"`
	Additions       int    `json:"additions,omitempty"`
	Deletions       int    `json:"deletions,omitempty"`
}

type DisplayHint struct {
	Type    string `json:"type"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
}
