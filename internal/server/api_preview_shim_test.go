package server

import (
	"bytes"
	"testing"
)

// The preview HTML must carry BOTH the theme shim and the runtime-error shim,
// injected inside <head> so they run before the app paints.
func TestInjectThemeShim_IncludesErrorShim(t *testing.T) {
	in := []byte("<html><head><title>x</title></head><body>hi</body></html>")
	out := injectThemeShim(in)

	for _, want := range [][]byte{
		[]byte("digi:theme-change"),  // theme shim present
		[]byte("digi:preview-error"), // error shim present
		[]byte("digi:nav"),           // nav shim present
		[]byte("addEventListener"),   // shims wire listeners
	} {
		if !bytes.Contains(out, want) {
			t.Fatalf("injected HTML missing %q", want)
		}
	}
	// Injected before </head> so it runs pre-paint.
	if bytes.Index(out, []byte("digi:preview-error")) > bytes.Index(out, []byte("</head>")) {
		t.Fatal("error shim injected after </head>")
	}
	// Original body content preserved (non-destructive).
	if !bytes.Contains(out, []byte("<body>hi</body>")) {
		t.Fatal("body content altered")
	}
}
