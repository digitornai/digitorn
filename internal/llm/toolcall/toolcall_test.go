package toolcall

import "testing"

func TestPlainProseNoMatch(t *testing.T) {
	for _, s := range []string{
		"",
		"Just a normal answer with no tools.",
		"Here is some `inline code` and a list:\n- a\n- b",
	} {
		r := Extract(s)
		if r.Matched() {
			t.Fatalf("plain prose matched a tool format: %q -> %+v", s, r)
		}
		if r.Cleaned != s {
			t.Fatalf("plain prose mutated: %q -> %q", s, r.Cleaned)
		}
	}
}

func TestAnthropicXML(t *testing.T) {
	in := "I'll list the files.\n<function_calls>\n<invoke name=\"filesystem__ls\">\n<parameter name=\"path\" string=\"true\">.</parameter>\n</invoke>\n</function_calls>"
	r := Extract(in)
	if r.Format != "anthropic_xml" {
		t.Fatalf("format=%q want anthropic_xml", r.Format)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "filesystem__ls" {
		t.Fatalf("calls=%+v", r.Calls)
	}
	if r.Calls[0].Arguments["path"] != "." {
		t.Fatalf("path arg=%v want '.'", r.Calls[0].Arguments["path"])
	}
	if r.Cleaned != "I'll list the files." {
		t.Fatalf("cleaned=%q (markup not stripped / prose lost)", r.Cleaned)
	}
}

func TestAnthropicXMLMultipleAndTypedArgs(t *testing.T) {
	in := `<function_calls>
<invoke name="fs__write"><parameter name="path">note.txt</parameter><parameter name="content">hi</parameter></invoke>
<invoke name="fs__read"><parameter name="lines">5</parameter><parameter name="raw">true</parameter></invoke>
</function_calls>`
	r := Extract(in)
	if len(r.Calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(r.Calls))
	}
	if r.Calls[1].Arguments["lines"] != float64(5) {
		t.Fatalf("lines should coerce to number, got %#v", r.Calls[1].Arguments["lines"])
	}
	if r.Calls[1].Arguments["raw"] != true {
		t.Fatalf("raw should coerce to bool, got %#v", r.Calls[1].Arguments["raw"])
	}
}

func TestAnthropicXMLTruncatedStream(t *testing.T) {
	// A reply cut off mid-stream before the closing tags must still parse and
	// must not leak the dangling opener into Cleaned.
	in := "ok\n<function_calls>\n<invoke name=\"fs__ls\"><parameter name=\"path\">.</parameter></invoke>"
	r := Extract(in)
	if len(r.Calls) != 1 || r.Calls[0].Name != "fs__ls" {
		t.Fatalf("calls=%+v", r.Calls)
	}
	if r.Cleaned != "ok" {
		t.Fatalf("cleaned=%q", r.Cleaned)
	}
}

func TestHermesTags(t *testing.T) {
	in := "Sure.\n<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}\n</tool_call>"
	r := Extract(in)
	if r.Format != "hermes_tags" {
		t.Fatalf("format=%q", r.Format)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "get_weather" || r.Calls[0].Arguments["city"] != "Paris" {
		t.Fatalf("calls=%+v", r.Calls)
	}
	if r.Cleaned != "Sure." {
		t.Fatalf("cleaned=%q", r.Cleaned)
	}
}

func TestDeepSeekTokens(t *testing.T) {
	in := "<｜tool▁calls▁begin｜><｜tool▁call▁begin｜>function<｜tool▁sep｜>filesystem__ls\n```json\n{\"path\": \".\"}\n```<｜tool▁call▁end｜><｜tool▁calls▁end｜>"
	r := Extract(in)
	if r.Format != "deepseek_tokens" {
		t.Fatalf("format=%q", r.Format)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "filesystem__ls" || r.Calls[0].Arguments["path"] != "." {
		t.Fatalf("calls=%+v", r.Calls)
	}
}

func TestFencedJSONNameArguments(t *testing.T) {
	in := "Running it:\n```json\n{\"name\": \"fs__read\", \"arguments\": {\"path\": \"a.txt\"}}\n```"
	r := Extract(in)
	if r.Format != "fenced_json" {
		t.Fatalf("format=%q", r.Format)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "fs__read" || r.Calls[0].Arguments["path"] != "a.txt" {
		t.Fatalf("calls=%+v", r.Calls)
	}
}

func TestBareJSONToolCallsArray(t *testing.T) {
	in := `{"tool_calls":[{"name":"a","arguments":{"x":1}},{"function":{"name":"b","arguments":"{\"y\":2}"}}]}`
	r := Extract(in)
	if len(r.Calls) != 2 || r.Calls[0].Name != "a" || r.Calls[1].Name != "b" {
		t.Fatalf("calls=%+v", r.Calls)
	}
	if r.Calls[1].Arguments["y"] != float64(2) {
		t.Fatalf("nested string-encoded arguments not decoded: %#v", r.Calls[1].Arguments)
	}
}

func TestFencedJSONIgnoresNonToolCode(t *testing.T) {
	// A plain code sample (no name/arguments/tool_call shape) must be left alone.
	in := "Example:\n```json\n{\"city\": \"Paris\", \"temp\": 21}\n```"
	r := Extract(in)
	if r.Matched() {
		t.Fatalf("non-tool JSON wrongly matched: %+v", r)
	}
}
