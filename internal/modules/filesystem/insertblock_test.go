package filesystem

import (
	"fmt"
	"strings"
	"testing"
)

// insert_after on a function signature must land AFTER the whole function body,
// not inside it (the real agent bug on isPalindrome/capitalize).
func TestEdit_InsertAfterFunctionBlock(t *testing.T) {
	m, ctx := hardenModule(t)
	src := "export function isPalindrome(s: string): boolean {\n" +
		"  const cleaned = s.toLowerCase();\n" +
		"  return cleaned === cleaned.split('').reverse().join('');\n" +
		"}\n"
	m.write(ctx, mustJSON(map[string]any{"path": "utils.ts", "content": src}))
	cap := "\nexport function capitalize(str: string): string {\n  return str[0].toUpperCase() + str.slice(1);\n}"
	r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "utils.ts", "insert_after": "export function isPalindrome", "new_string": cap,
	}))
	if err != nil || !r.Success {
		t.Fatalf("insert_after block: err=%v result=%v", err, r.Error)
	}
	got := fmt.Sprint(rrData(m, ctx, "utils.ts"))
	// isPalindrome's return must come BEFORE capitalize's declaration.
	ip := strings.Index(got, "return cleaned")
	cp := strings.Index(got, "function capitalize")
	if ip < 0 || cp < 0 || ip > cp {
		t.Fatalf("capitalize was inserted inside isPalindrome body:\n%s", got)
	}
}

// a non-block anchor keeps the plain after-the-line behaviour.
func TestEdit_InsertAfterPlainLine(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "a.txt", "content": "one\ntwo\nthree\n"}))
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "a.txt", "insert_after": "two", "new_string": "INSERTED"}))
	if err != nil || !r.Success {
		t.Fatalf("insert_after plain: %v %v", err, r.Error)
	}
	got := fmt.Sprint(rrData(m, ctx, "a.txt"))
	if !strings.Contains(got, "two") || strings.Index(got, "INSERTED") < strings.Index(got, "two") {
		t.Fatalf("plain insert_after misplaced: %s", got)
	}
}
