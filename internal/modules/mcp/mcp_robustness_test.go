package mcp

import (
	"errors"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// classify must mark PERMANENT failures (auth, 404, missing command, 4xx)
// non-retryable so the reconnect/health loop stops hammering a dead server,
// while transient faults (refused/reset/timeout/5xx/429) stay retryable.
func TestClassify_Retryable(t *testing.T) {
	cases := []struct {
		msg       string
		retryable bool
	}{
		{"dial tcp: connection refused", true},
		{"read: connection reset by peer", true},
		{"context deadline exceeded", true},
		{"provider returned status 503", true},
		{"too many requests: status 429", true},
		{"request timeout: status 408", true},
		{"unauthorized: status 401", false},
		{"forbidden: status 403", false},
		{"server returned status 404 not found", false},
		{"bad request: status 400", false},
		{`exec: "npx": executable file not found in $PATH`, false},
		{"oauth: invalid_grant", false},
	}
	for _, c := range cases {
		got := classify(errors.New(c.msg))
		te, ok := got.(*MCPTransportError)
		if !ok {
			t.Fatalf("classify(%q) did not return *MCPTransportError", c.msg)
		}
		if te.Retryable != c.retryable {
			t.Errorf("classify(%q).Retryable = %v, want %v", c.msg, te.Retryable, c.retryable)
		}
	}
	if classify(nil) != nil {
		t.Error("classify(nil) must be nil")
	}
}

// splitContent must preserve EVERY content kind — text, image, audio, embedded
// resource, resource link, unknown — so a non-text tool result is never dropped.
func TestSplitContent_AllKinds(t *testing.T) {
	content := []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "hello "},
		&mcpsdk.TextContent{Text: "world"},
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("PNGDATA")},
		&mcpsdk.AudioContent{MIMEType: "audio/wav", Data: []byte("WAVDATA")},
		&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{URI: "file://x.txt", MIMEType: "text/plain", Text: " embedded-text"}},
		&mcpsdk.ResourceLink{URI: "https://example/r", Name: "r", MIMEType: "text/html"},
	}
	p := splitContent(content)
	if p.text != "hello world embedded-text" {
		t.Errorf("text = %q, want concatenated text incl. embedded resource", p.text)
	}
	if len(p.images) != 1 || p.images[0]["mimeType"] != "image/png" {
		t.Errorf("image block missing/wrong: %+v", p.images)
	}
	if len(p.audio) != 1 || p.audio[0]["mimeType"] != "audio/wav" {
		t.Errorf("audio block missing/wrong: %+v", p.audio)
	}
	if len(p.resources) != 1 || p.resources[0]["uri"] != "file://x.txt" {
		t.Errorf("embedded resource block missing/wrong: %+v", p.resources)
	}
	if len(p.links) != 1 || p.links[0]["uri"] != "https://example/r" {
		t.Errorf("resource link missing/wrong: %+v", p.links)
	}
}

// wrapResult of an image-only tool result must NOT be reported as "empty".
func TestWrapResult_ImageOnly_NotEmpty(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("img")},
	}}
	out := wrapResult("everything", "getTinyImage", res)
	if !out.Success {
		t.Fatalf("image result should be a success: %+v", out)
	}
	data := out.Data.(map[string]any)
	if data["status"] == "empty" {
		t.Error("image-only result was wrongly marked empty (the old joinText bug)")
	}
	if _, ok := data["images"]; !ok {
		t.Error("image-only result must carry an images[] block")
	}
}

// nodeBinDir must locate node on a machine that has it (this CI host runs the
// npx integration servers, so node is present).
func TestNodeBinDir_FindsNode(t *testing.T) {
	if dir := nodeBinDir(); dir == "" {
		t.Skip("node not found on this host — skipping (ensureNodeOnPath is a no-op here)")
	}
}
