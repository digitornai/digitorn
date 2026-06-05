package filesystem

import (
	"fmt"
	"strings"
	"testing"
)

func TestComputeDiff_SingleLineChange(t *testing.T) {
	old := "alpha\nbeta\ngamma\n"
	neu := "alpha\nBETA\ngamma\n"
	d := computeDiff("f.txt", old, neu)
	if d.Added != 1 || d.Removed != 1 {
		t.Fatalf("counts: +%d −%d, want +1 −1", d.Added, d.Removed)
	}
	if d.Previous != old || d.New != neu {
		t.Error("previous/new content must be carried for a small file")
	}
	if !strings.Contains(d.Unified, "-beta") || !strings.Contains(d.Unified, "+BETA") {
		t.Errorf("unified diff missing the change:\n%s", d.Unified)
	}
	if !strings.Contains(d.Unified, " alpha") || !strings.Contains(d.Unified, " gamma") {
		t.Errorf("unified diff missing context lines:\n%s", d.Unified)
	}
	if !strings.HasPrefix(d.Unified, "--- a/f.txt\n+++ b/f.txt\n@@ ") {
		t.Errorf("unified diff header wrong:\n%s", d.Unified)
	}
	if d.Summary != "+1 −1" {
		t.Errorf("summary = %q, want +1 −1", d.Summary)
	}
}

func TestComputeDiff_PureInsertionAndDeletion(t *testing.T) {
	ins := computeDiff("f", "a\nb\n", "a\nx\nb\n")
	if ins.Added != 1 || ins.Removed != 0 {
		t.Errorf("insertion: +%d −%d, want +1 −0", ins.Added, ins.Removed)
	}
	if !strings.Contains(ins.Unified, "+x") {
		t.Errorf("insertion diff:\n%s", ins.Unified)
	}
	del := computeDiff("f", "a\nx\nb\n", "a\nb\n")
	if del.Added != 0 || del.Removed != 1 {
		t.Errorf("deletion: +%d −%d, want +0 −1", del.Added, del.Removed)
	}
	if !strings.Contains(del.Unified, "-x") {
		t.Errorf("deletion diff:\n%s", del.Unified)
	}
}

func TestComputeDiff_LocalizedInLargeFile(t *testing.T) {
	// 500 identical lines, change one in the middle : the hunk must be local
	// (a few context lines), NOT the whole file.
	var ob, nb strings.Builder
	for i := 0; i < 500; i++ {
		if i == 250 {
			ob.WriteString("OLD_MIDDLE\n")
			nb.WriteString("NEW_MIDDLE\n")
		} else {
			ob.WriteString("line\n")
			nb.WriteString("line\n")
		}
	}
	d := computeDiff("big.txt", ob.String(), nb.String())
	if d.Added != 1 || d.Removed != 1 {
		t.Fatalf("counts +%d −%d", d.Added, d.Removed)
	}
	lines := strings.Count(d.Unified, "\n")
	if lines > 12 { // header(2) + hunk header(1) + ~7 context/change : must stay tiny
		t.Errorf("hunk not localized — %d lines emitted for a 1-line change:\n%s", lines, d.Unified)
	}
	if !strings.Contains(d.Unified, "@@ -248,") && !strings.Contains(d.Unified, "@@ -249,") && !strings.Contains(d.Unified, "@@ -250,") {
		t.Errorf("hunk header line number off:\n%s", d.Unified)
	}
}

func TestComputeDiff_OverCapSummaryOnly(t *testing.T) {
	big := strings.Repeat("x\n", (diffContentCap/2)+10) // > cap
	d := computeDiff("huge.txt", big, big+"more\n")
	if d.Unified != "" || d.Previous != "" || d.New != "" {
		t.Error("over-cap diff must carry NO unified diff and NO contents")
	}
	if !strings.Contains(d.Summary, "diff omitted") {
		t.Errorf("over-cap summary should say omitted: %q", d.Summary)
	}
}

func TestComputeDiff_NoChange(t *testing.T) {
	if d := computeDiff("f", "same\n", "same\n"); !d.empty() {
		t.Errorf("identical content must produce an empty diff, got %+v", d)
	}
}

// TestComputeDiff_UnifiedApplies sanity-checks that the emitted hunk line counts
// match the actual lines, i.e. the @@ header is internally consistent.
func TestComputeDiff_HeaderCountsConsistent(t *testing.T) {
	d := computeDiff("f", "a\nb\nc\nd\ne\n", "a\nB\nc\nD\ne\n")
	for _, line := range strings.Split(d.Unified, "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		// crude parse : "@@ -os,oc +ns,nc @@"
		var os, oc, ns, nc int
		if _, err := fmt.Sscanf(line, "@@ -%d,%d +%d,%d @@", &os, &oc, &ns, &nc); err != nil {
			t.Fatalf("bad hunk header %q: %v", line, err)
		}
		if oc <= 0 || nc <= 0 {
			t.Errorf("hunk header counts must be positive: %q", line)
		}
	}
}
