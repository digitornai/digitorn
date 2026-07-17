package lsp

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspDiagnostic struct {
	Range    lspRange        `json:"range"`
	Severity int             `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  string          `json:"message"`
}

type publishDiagnosticsParams struct {
	URI         string          `json:"uri"`
	Version     int             `json:"version"`
	Diagnostics []lspDiagnostic `json:"diagnostics"`
}

type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

func severityName(s int) string {
	switch s {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "error"
	}
}

func toDiagnostics(file string, raw []lspDiagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(raw))
	for _, d := range raw {
		code := ""
		if len(d.Code) > 0 && string(d.Code) != "null" {
			code = string(d.Code)
			if u, err := jsonString(d.Code); err == nil {
				code = u
			}
		}
		out = append(out, Diagnostic{
			File:     file,
			Line:     d.Range.Start.Line + 1,
			Column:   d.Range.Start.Character + 1,
			Severity: severityName(d.Severity),
			Code:     code,
			Source:   d.Source,
			Message:  d.Message,
		})
	}
	return out
}

func jsonString(raw json.RawMessage) (string, error) {
	var s string
	err := json.Unmarshal(raw, &s)
	return s, err
}

func toDiagnosticsBytes(file string, raw []lspDiagnostic, content, encoding string) []Diagnostic {
	out := toDiagnostics(file, raw)
	if encoding == "utf-8" || content == "" {
		return out
	}
	lines := strings.Split(content, "\n")
	for i := range out {
		if i >= len(raw) {
			break
		}
		line0 := raw[i].Range.Start.Line
		if line0 < 0 || line0 >= len(lines) {
			continue
		}
		utf16Col := raw[i].Range.Start.Character
		out[i].Column = utf16ColumnToByteColumn(lines[line0], utf16Col) + 1
	}
	return out
}

func utf16ColumnToByteColumn(line string, utf16Col int) int {
	if utf16Col <= 0 {
		return 0
	}
	byteOffset := 0
	utf16Count := 0
	for _, r := range line {
		if utf16Count >= utf16Col {
			break
		}
		size := 1
		if r > 0xFFFF {
			size = 2
		}
		utf16Count += size
		byteOffset += utf8.RuneLen(r)
	}
	return byteOffset
}
