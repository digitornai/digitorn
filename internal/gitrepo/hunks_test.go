package gitrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// A 6-line baseline with two independent edits (line 2 and line 5), each its
// own hunk with one line of context — the canonical "approve/reject one of two
// hunks" case.
const twoHunkDiff = `--- a/foo.txt
+++ b/foo.txt
@@ -1,3 +1,3 @@
 line1
-line2
+LINE2
 line3
@@ -4,3 +4,3 @@
 line4
-line5
+LINE5
 line6`

var baseline6 = []string{"line1", "line2", "line3", "line4", "line5", "line6"}

// hashMatchesWebFormula locks the hash to the EXACT formula the web + legacy
// Python daemon use: sha256(header + "\n" + body.join("\n")).hex()[:12].
func TestHunkHash_MatchesWebFormula(t *testing.T) {
	header := "@@ -1,3 +1,3 @@"
	body := []string{" line1", "-line2", "+LINE2", " line3"}
	sum := sha256.Sum256([]byte(header + "\n" + strings.Join(body, "\n")))
	want := hex.EncodeToString(sum[:])[:12]
	if got := hunkHashOf(header, body); got != want {
		t.Fatalf("hash %q != formula %q", got, want)
	}
	if len(want) != 12 {
		t.Fatalf("hash must be 12 chars, got %d", len(want))
	}
}

func TestParseUnifiedHunks(t *testing.T) {
	hs := parseUnifiedHunks(twoHunkDiff)
	if len(hs) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(hs))
	}
	if hs[0].OldStart != 1 || hs[0].OldLen != 3 || hs[0].NewStart != 1 || hs[0].NewLen != 3 {
		t.Fatalf("hunk0 ranges wrong: %+v", hs[0])
	}
	if hs[1].OldStart != 4 {
		t.Fatalf("hunk1 OldStart = %d, want 4", hs[1].OldStart)
	}
	// File markers must NOT leak into the body.
	for _, h := range hs {
		for _, b := range h.Body {
			if strings.HasPrefix(b, "---") || strings.HasPrefix(b, "+++") {
				t.Fatalf("file marker leaked into hunk body: %q", b)
			}
		}
	}
	if hs[0].Hash == "" || hs[0].Hash == hs[1].Hash {
		t.Fatalf("hashes must be present and distinct: %q / %q", hs[0].Hash, hs[1].Hash)
	}
}

func TestApplyHunks_AllReproducesCurrent(t *testing.T) {
	hs := parseUnifiedHunks(twoHunkDiff)
	got, err := applyHunks(baseline6, hs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line1", "LINE2", "line3", "line4", "LINE5", "line6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("apply all = %v, want %v", got, want)
	}
}

func TestApplyHunks_SubsetFirstOnly(t *testing.T) {
	hs := parseUnifiedHunks(twoHunkDiff)
	got, err := applyHunks(baseline6, []diffHunk{hs[0]})
	if err != nil {
		t.Fatal(err)
	}
	// Only line 2 changes; line 5 stays baseline.
	want := []string{"line1", "LINE2", "line3", "line4", "line5", "line6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("apply hunk0 = %v, want %v", got, want)
	}
}

func TestApplyHunks_SubsetSecondOnly(t *testing.T) {
	hs := parseUnifiedHunks(twoHunkDiff)
	got, err := applyHunks(baseline6, []diffHunk{hs[1]})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line1", "line2", "line3", "line4", "LINE5", "line6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("apply hunk1 = %v, want %v", got, want)
	}
}

// Reject scenario: keep everything EXCEPT the selected hunk = apply the others.
func TestApplyHunks_RejectIsApplyOthers(t *testing.T) {
	hs := parseUnifiedHunks(twoHunkDiff)
	_, others, missing := selectHunks(hs, []string{hs[0].Hash})
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	// Rejecting hunk0 (line2 edit) → worktree = baseline + hunk1 only.
	got, err := applyHunks(baseline6, others)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line1", "line2", "line3", "line4", "LINE5", "line6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reject hunk0 = %v, want %v", got, want)
	}
}

func TestApplyHunks_StaleContextRefused(t *testing.T) {
	hs := parseUnifiedHunks(twoHunkDiff)
	// Baseline drifted (line1 changed under us): the context no longer matches.
	drifted := []string{"DIFFERENT", "line2", "line3", "line4", "line5", "line6"}
	if _, err := applyHunks(drifted, []diffHunk{hs[0]}); err == nil {
		t.Fatal("expected a stale-context error, got nil (would corrupt the file)")
	}
}

func TestApplyHunks_PureInsertion(t *testing.T) {
	// Insert two lines after line 2: @@ -2,0 +3,2 @@ (oldLen 0).
	diff := "@@ -2,0 +3,2 @@\n+inserted-a\n+inserted-b"
	hs := parseUnifiedHunks(diff)
	if len(hs) != 1 || hs[0].OldLen != 0 {
		t.Fatalf("bad parse: %+v", hs)
	}
	got, err := applyHunks(baseline6, hs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line1", "line2", "inserted-a", "inserted-b", "line3", "line4", "line5", "line6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("insertion = %v, want %v", got, want)
	}
}

func TestSelectHunks_MissingReported(t *testing.T) {
	hs := parseUnifiedHunks(twoHunkDiff)
	sel, _, missing := selectHunks(hs, []string{hs[1].Hash, "deadbeefdead"})
	if len(sel) != 1 || sel[0].Hash != hs[1].Hash {
		t.Fatalf("bad selection: %+v", sel)
	}
	if len(missing) != 1 || missing[0] != "deadbeefdead" {
		t.Fatalf("missing not reported: %v", missing)
	}
}

func TestSplitJoinLines_RoundTrip(t *testing.T) {
	cases := []string{"a\nb\nc\n", "a\nb\nc", "", "\n", "single", "x\n\ny\n"}
	for _, c := range cases {
		lines, tn := splitLines(c)
		if got := joinLines(lines, tn); got != c {
			t.Fatalf("round-trip %q -> %q", c, got)
		}
	}
}
