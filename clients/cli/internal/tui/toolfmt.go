package tui

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolfmt.go turns a tool result into clean, human-readable text — never raw
// JSON. The daemon serialises each tool's Data as JSON (e.g. bash → {stdout,
// stderr,exit_code}, glob → {files,count}), which is noise in a chip. We parse
// the known shapes back and render what a person actually wants to see :
// stdout for a shell run, the file list for a glob, "file:line text" for grep.
// Unknown shapes fall through to the raw text untouched (graceful).

// formatToolResult is the single entry point used by the tool_result handler.
// On a failed call the error text is the detail worth showing.
func formatToolResult(p map[string]any) string {
	if e := payloadStr(p, "error"); e != "" {
		return e
	}
	raw := payloadPartsText(p)
	if raw == "" {
		raw = payloadStr(p, "output")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	switch stripModulePrefix(toolDisplayName(p)) {
	case "bash", "run", "exec", "shell":
		return formatShellOutput(raw)
	case "glob":
		return formatGlobOutput(raw)
	case "grep", "search":
		return formatGrepOutput(raw)
	case "write", "edit", "multi_edit":
		return formatMutationOutput(raw)
	case "background_run":
		return formatBackgroundOutput(raw)
	case "run_parallel":
		return formatParallelOutput(raw)
	default:
		return raw
	}
}

// formatParallelOutput renders run_parallel's {results:[{name,status,error}]}
// as a clean GROUP — one line per sub-tool with a status glyph and its name —
// instead of the raw JSON, so you see what ran in parallel and how each fared.
func formatParallelOutput(raw string) string {
	m, ok := asJSONObject(raw)
	if !ok {
		return raw
	}
	arr, ok := m["results"].([]any)
	if !ok {
		return raw
	}
	var lines []string
	for _, e := range arr {
		r, _ := e.(map[string]any)
		if r == nil {
			continue
		}
		name := stripModulePrefix(mapStr(r, "name"))
		glyph := "·"
		switch mapStr(r, "status") {
		case "completed", "done", "ok", "success":
			glyph = "✓"
		case "errored", "error", "failed", "cancelled":
			glyph = "✗"
		}
		line := glyph + " " + name
		if e := mapStr(r, "error"); e != "" {
			line += " — " + oneLine(e, 60)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return raw
	}
	return strings.Join(lines, "\n")
}

// formatBackgroundOutput renders a background_run snapshot ({name, state,
// task_id, …}) as a clean "<tool> · <state> · <id>" line instead of the raw
// JSON the meta-tool returns.
func formatBackgroundOutput(raw string) string {
	m, ok := asJSONObject(raw)
	if !ok {
		return raw
	}
	var parts []string
	if name := mapStr(m, "name"); name != "" {
		parts = append(parts, strings.ReplaceAll(name, "__", "."))
	}
	if state := mapStr(m, "state"); state != "" {
		parts = append(parts, state)
	} else if status := mapStr(m, "status"); status != "" {
		parts = append(parts, status)
	}
	if id := mapStr(m, "task_id"); id != "" {
		if i := strings.IndexByte(id, '-'); i > 0 {
			id = id[:i]
		}
		parts = append(parts, id)
	}
	if len(parts) == 0 {
		return raw
	}
	return strings.Join(parts, " · ")
}

// asJSONObject parses raw into a generic object, ok=false when it isn't JSON
// (e.g. read returns a plain string) so the caller can fall back to raw.
func asJSONObject(raw string) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, false
	}
	return m, true
}

// formatShellOutput shows the command's stdout (then stderr), not the
// {stdout,stderr,exit_code,…} envelope. A non-zero exit is noted ; a silent
// command reads as "(no output)".
func formatShellOutput(raw string) string {
	m, ok := asJSONObject(raw)
	if !ok {
		return raw
	}
	stdout := strings.TrimRight(mapStr(m, "stdout"), "\n")
	stderr := strings.TrimRight(mapStr(m, "stderr"), "\n")
	var b strings.Builder
	if stdout != "" {
		b.WriteString(stdout)
	}
	if stderr != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(stderr)
	}
	if code, okc := mapInt(m, "exit_code"); okc && code != 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "exit %d", code)
	}
	if b.Len() == 0 {
		return "(no output)"
	}
	return b.String()
}

// formatGlobOutput lists the matched files, one per line, with a trailing "…"
// when the daemon truncated the set.
func formatGlobOutput(raw string) string {
	m, ok := asJSONObject(raw)
	if !ok {
		return raw
	}
	files := mapStrSlice(m["files"])
	if len(files) == 0 {
		if c, okc := mapInt(m, "count"); okc {
			return fmt.Sprintf("%d files", c)
		}
		return "(no matches)"
	}
	out := strings.Join(files, "\n")
	if truncated, _ := m["truncated"].(bool); truncated {
		out += "\n…"
	}
	return out
}

// formatGrepOutput renders grep's three modes : matches → "file:line  text"
// lines, files → a file list, count → "N matches".
func formatGrepOutput(raw string) string {
	m, ok := asJSONObject(raw)
	if !ok {
		return raw
	}
	if arr, isArr := m["matches"].([]any); isArr {
		var lines []string
		for _, e := range arr {
			mm, _ := e.(map[string]any)
			if mm == nil {
				continue
			}
			file := mapStr(mm, "file")
			ln, _ := mapInt(mm, "line")
			text := strings.TrimSpace(mapStr(mm, "text"))
			lines = append(lines, fmt.Sprintf("%s:%d  %s", file, ln, text))
		}
		if truncated, _ := m["truncated"].(bool); truncated {
			lines = append(lines, "…")
		}
		if len(lines) > 0 {
			return strings.Join(lines, "\n")
		}
	}
	if files := mapStrSlice(m["files"]); len(files) > 0 {
		return strings.Join(files, "\n")
	}
	if c, okc := mapInt(m, "count"); okc {
		return fmt.Sprintf("%d matches", c)
	}
	return "(no matches)"
}

// formatMutationOutput summarises a write/edit when there's no diff to show
// (an unchanged write, or a result that didn't carry a diff). With a diff the
// chip renders that instead and this is never reached.
func formatMutationOutput(raw string) string {
	m, ok := asJSONObject(raw)
	if !ok {
		return raw
	}
	if action := mapStr(m, "action"); action != "" {
		if b, okb := mapInt(m, "bytes"); okb {
			return fmt.Sprintf("%s · %d bytes", action, b)
		}
		return action
	}
	if r, okr := mapInt(m, "replacements"); okr {
		if r == 1 {
			return "1 replacement"
		}
		return fmt.Sprintf("%d replacements", r)
	}
	return raw
}

// mapInt reads a numeric field, tolerating JSON's float64 decoding.
func mapInt(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// mapStrSlice coerces a JSON array field into []string.
func mapStrSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
