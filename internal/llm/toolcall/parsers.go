package toolcall

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ---- shared helpers -------------------------------------------------------

// coerceValue decodes a raw parameter value : a JSON scalar/object/array is
// returned as the decoded type, anything else as the trimmed literal string.
// So `.` stays ".", `5` becomes 5, `true` becomes true, `{"a":1}` becomes a map.
func coerceValue(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

// objToArgs normalises an "arguments"/"parameters" field that may be a JSON
// object or a JSON-encoded string into a map.
func objToArgs(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return map[string]any{}
		}
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil {
			return m
		}
	}
	return map[string]any{}
}

// callFromObject extracts a Call from a decoded JSON object in any of the
// common shapes : {name,arguments} · {name,parameters} · {function:{name,...}}.
func callFromObject(m map[string]any) (Call, bool) {
	if fn, ok := m["function"].(map[string]any); ok {
		m = fn
	}
	name, _ := m["name"].(string)
	if name == "" {
		return Call{}, false
	}
	args := m["arguments"]
	if args == nil {
		args = m["parameters"]
	}
	return Call{Name: name, Arguments: objToArgs(args)}, true
}

// ---- Anthropic / Claude XML : <function_calls><invoke name=…><parameter …> --

var (
	reInvoke      = regexp.MustCompile(`(?s)<invoke\s+name="([^"]+)"\s*>(.*?)</invoke>`)
	reParameter   = regexp.MustCompile(`(?s)<parameter\s+name="([^"]+)"[^>]*>(.*?)</parameter>`)
	reFnCallsBlk  = regexp.MustCompile(`(?s)<function_calls>.*?</function_calls>`)
	reInvokeBlk   = regexp.MustCompile(`(?s)<invoke\s.*?</invoke>`)
	reFnCallsOpen = regexp.MustCompile(`(?s)<function_calls>.*$`)
)

type anthropicXML struct{}

func (anthropicXML) Name() string { return "anthropic_xml" }

func (anthropicXML) Parse(content string) ([]Call, string, bool) {
	invokes := reInvoke.FindAllStringSubmatch(content, -1)
	if len(invokes) == 0 {
		return nil, content, false
	}
	calls := make([]Call, 0, len(invokes))
	for _, inv := range invokes {
		name := strings.TrimSpace(inv[1])
		if name == "" {
			continue
		}
		args := map[string]any{}
		for _, p := range reParameter.FindAllStringSubmatch(inv[2], -1) {
			if key := strings.TrimSpace(p[1]); key != "" {
				args[key] = coerceValue(p[2])
			}
		}
		calls = append(calls, Call{Name: name, Arguments: args})
	}
	if len(calls) == 0 {
		return nil, content, false
	}
	cleaned := reFnCallsBlk.ReplaceAllString(content, "")
	cleaned = reInvokeBlk.ReplaceAllString(cleaned, "")
	// A streamed reply may be cut off before the closing tag : drop the dangling
	// opener so it never leaks into the visible message.
	cleaned = reFnCallsOpen.ReplaceAllString(cleaned, "")
	return calls, cleaned, true
}

// ---- DeepSeek special-token format ----------------------------------------
// <｜tool▁calls▁begin｜><｜tool▁call▁begin｜>function<｜tool▁sep｜>NAME```json
// {args}```<｜tool▁call▁end｜>…<｜tool▁calls▁end｜>  (｜=U+FF5C, ▁=U+2581)

var (
	reDSCall = regexp.MustCompile(`(?s)<｜tool▁call▁begin｜>(.*?)<｜tool▁call▁end｜>`)
	reDSName = regexp.MustCompile(`(?s)<｜tool▁sep｜>\s*([^\n` + "`" + `]+)`)
	reDSArgs = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")
	reDSBlk  = regexp.MustCompile(`(?s)<｜tool▁calls▁begin｜>.*?<｜tool▁calls▁end｜>`)
	reDSOpen = regexp.MustCompile(`(?s)<｜tool▁calls▁begin｜>.*$`)
)

type deepseekTokens struct{}

func (deepseekTokens) Name() string { return "deepseek_tokens" }

func (deepseekTokens) Parse(content string) ([]Call, string, bool) {
	blocks := reDSCall.FindAllStringSubmatch(content, -1)
	if len(blocks) == 0 {
		return nil, content, false
	}
	calls := make([]Call, 0, len(blocks))
	for _, b := range blocks {
		body := b[1]
		nm := reDSName.FindStringSubmatch(body)
		if nm == nil {
			continue
		}
		name := strings.TrimSpace(nm[1])
		if name == "" {
			continue
		}
		args := map[string]any{}
		if a := reDSArgs.FindStringSubmatch(body); a != nil {
			args = objToArgs(strings.TrimSpace(a[1]))
		}
		calls = append(calls, Call{Name: name, Arguments: args})
	}
	if len(calls) == 0 {
		return nil, content, false
	}
	cleaned := reDSBlk.ReplaceAllString(content, "")
	cleaned = reDSOpen.ReplaceAllString(cleaned, "")
	return calls, cleaned, true
}

// ---- Hermes / Qwen : <tool_call>{"name":…,"arguments":{…}}</tool_call> -----

var (
	reHermes     = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
	reHermesBlk  = regexp.MustCompile(`(?s)<tool_call>.*?</tool_call>`)
	reHermesOpen = regexp.MustCompile(`(?s)<tool_call>.*$`)
)

type hermesTags struct{}

func (hermesTags) Name() string { return "hermes_tags" }

func (hermesTags) Parse(content string) ([]Call, string, bool) {
	matches := reHermes.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil, content, false
	}
	calls := make([]Call, 0, len(matches))
	for _, m := range matches {
		var obj map[string]any
		if json.Unmarshal([]byte(m[1]), &obj) != nil {
			continue
		}
		if c, ok := callFromObject(obj); ok {
			calls = append(calls, c)
		}
	}
	if len(calls) == 0 {
		return nil, content, false
	}
	cleaned := reHermesBlk.ReplaceAllString(content, "")
	cleaned = reHermesOpen.ReplaceAllString(cleaned, "")
	return calls, cleaned, true
}

// ---- Fenced / bare JSON tool call (last resort) ---------------------------
// A ```json fenced block (or the whole message) whose object carries a tool
// shape : {name,arguments|parameters} or {tool_call|tool_calls}. Conservative
// on purpose — a plain code sample without those keys is left untouched.

var reFence = regexp.MustCompile("(?s)```(?:json|tool_call|tool_code)?\\s*(\\{.*?\\}|\\[.*?\\])\\s*```")

type fencedJSON struct{}

func (fencedJSON) Name() string { return "fenced_json" }

func (fencedJSON) Parse(content string) ([]Call, string, bool) {
	candidates := reFence.FindAllStringSubmatch(content, -1)
	var raws []string
	for _, c := range candidates {
		raws = append(raws, c[1])
	}
	// Bare top-level JSON (no fence) is also accepted when the whole trimmed
	// message is a single object/array — common for "JSON mode" tool emulation.
	if len(raws) == 0 {
		t := strings.TrimSpace(content)
		if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			raws = append(raws, t)
		}
	}
	if len(raws) == 0 {
		return nil, content, false
	}
	var calls []Call
	for _, raw := range raws {
		calls = append(calls, jsonToolCalls(raw)...)
	}
	if len(calls) == 0 {
		return nil, content, false
	}
	cleaned := reFence.ReplaceAllString(content, "")
	return calls, cleaned, true
}

// jsonToolCalls reads one JSON document in any tool-bearing shape and returns
// every Call it carries. Returns nil when the JSON has no tool shape.
func jsonToolCalls(raw string) []Call {
	var top any
	if json.Unmarshal([]byte(raw), &top) != nil {
		return nil
	}
	switch t := top.(type) {
	case []any:
		var out []Call
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				if c, ok := callFromObject(m); ok {
					out = append(out, c)
				}
			}
		}
		return out
	case map[string]any:
		if arr, ok := t["tool_calls"].([]any); ok {
			var out []Call
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok {
					if c, ok := callFromObject(m); ok {
						out = append(out, c)
					}
				}
			}
			return out
		}
		if one, ok := t["tool_call"].(map[string]any); ok {
			if c, ok := callFromObject(one); ok {
				return []Call{c}
			}
		}
		if c, ok := callFromObject(t); ok {
			return []Call{c}
		}
	}
	return nil
}
