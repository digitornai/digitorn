package filesystem

import (
	"fmt"
	"strings"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

const (
	diffContext    = 3
	diffContentCap = 256 << 10
	diffMaxLCS     = 2000
)

type fileDiff struct {
	Unified  string
	Summary  string
	Previous string
	New      string
	Added    int
	Removed  int
}

func (d fileDiff) empty() bool {
	return d.Added == 0 && d.Removed == 0 && d.Unified == "" && d.Summary == ""
}

func diffView(path, oldStr, newStr string) *tool.DiffView {
	d := computeDiff(path, oldStr, newStr)
	if d.empty() {
		return nil
	}
	return &tool.DiffView{
		Unified:         d.Unified,
		Summary:         d.Summary,
		PreviousContent: d.Previous,
		NewContent:      d.New,
		Additions:       d.Added,
		Deletions:       d.Removed,
	}
}

func computeDiff(path, oldStr, newStr string) fileDiff {
	if oldStr == newStr {
		return fileDiff{}
	}
	oldLines := splitLines(oldStr)
	newLines := splitLines(newStr)

	p := 0
	for p < len(oldLines) && p < len(newLines) && oldLines[p] == newLines[p] {
		p++
	}
	s := 0
	for s < len(oldLines)-p && s < len(newLines)-p &&
		oldLines[len(oldLines)-1-s] == newLines[len(newLines)-1-s] {
		s++
	}
	midOld := oldLines[p : len(oldLines)-s]
	midNew := newLines[p : len(newLines)-s]

	if len(oldStr) > diffContentCap || len(newStr) > diffContentCap {
		return fileDiff{
			Summary: fmt.Sprintf("large change: +%d −%d lines (diff omitted, file over %d KB)", len(midNew), len(midOld), diffContentCap>>10),
			Added:   len(midNew),
			Removed: len(midOld),
		}
	}

	var ops []dop
	if len(midOld) > diffMaxLCS || len(midNew) > diffMaxLCS {
		ops = coarseOps(midOld, midNew)
	} else {
		ops = lcsOps(midOld, midNew)
	}

	unified, added, removed := formatUnified(path, oldLines, newLines, p, s, ops)
	return fileDiff{
		Unified:  unified,
		Summary:  summarize(added, removed),
		Previous: oldStr,
		New:      newStr,
		Added:    added,
		Removed:  removed,
	}
}

func summarize(added, removed int) string {
	switch {
	case added > 0 && removed > 0:
		return fmt.Sprintf("+%d −%d", added, removed)
	case added > 0:
		return fmt.Sprintf("+%d", added)
	case removed > 0:
		return fmt.Sprintf("−%d", removed)
	default:
		return "no change"
	}
}

type dop struct {
	kind byte
	text string
}

func coarseOps(a, b []string) []dop {
	ops := make([]dop, 0, len(a)+len(b))
	for _, l := range a {
		ops = append(ops, dop{'-', l})
	}
	for _, l := range b {
		ops = append(ops, dop{'+', l})
	}
	return ops
}

func lcsOps(a, b []string) []dop {
	na, nb := len(a), len(b)
	dp := make([][]int, na+1)
	for i := range dp {
		dp[i] = make([]int, nb+1)
	}
	for i := na - 1; i >= 0; i-- {
		for j := nb - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	ops := make([]dop, 0, na+nb)
	i, j := 0, 0
	for i < na && j < nb {
		switch {
		case a[i] == b[j]:
			ops = append(ops, dop{'=', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, dop{'-', a[i]})
			i++
		default:
			ops = append(ops, dop{'+', b[j]})
			j++
		}
	}
	for ; i < na; i++ {
		ops = append(ops, dop{'-', a[i]})
	}
	for ; j < nb; j++ {
		ops = append(ops, dop{'+', b[j]})
	}
	return ops
}

type rec struct {
	kind         byte
	text         string
	oldNo, newNo int
}

func formatUnified(path string, oldLines, newLines []string, p, s int, midOps []dop) (unified string, added, removed int) {
	full := make([]dop, 0, p+len(midOps)+s)
	for i := 0; i < p; i++ {
		full = append(full, dop{'=', oldLines[i]})
	}
	full = append(full, midOps...)
	for i := len(oldLines) - s; i < len(oldLines); i++ {
		full = append(full, dop{'=', oldLines[i]})
	}

	recs := make([]rec, len(full))
	oldNo, newNo := 1, 1
	for k, op := range full {
		recs[k] = rec{op.kind, op.text, oldNo, newNo}
		switch op.kind {
		case '=':
			oldNo++
			newNo++
		case '-':
			oldNo++
			removed++
		case '+':
			newNo++
			added++
		}
	}
	if added == 0 && removed == 0 {
		return "", 0, 0
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)

	n := len(recs)
	i := 0
	for i < n {
		if recs[i].kind == '=' {
			i++
			continue
		}
		hs := i - diffContext
		if hs < 0 {
			hs = 0
		}
		last := i
		j := i
		gap := 0
		for j < n {
			if recs[j].kind != '=' {
				last = j
				gap = 0
				j++
				continue
			}
			gap++
			if gap > diffContext {
				break
			}
			j++
		}
		he := last + diffContext + 1
		if he > n {
			he = n
		}
		emitHunk(&b, recs, hs, he)
		i = he
	}
	return b.String(), added, removed
}

func emitHunk(b *strings.Builder, recs []rec, start, end int) {
	var oldCount, newCount int
	for _, r := range recs[start:end] {
		if r.kind == '=' || r.kind == '-' {
			oldCount++
		}
		if r.kind == '=' || r.kind == '+' {
			newCount++
		}
	}
	oldStart := recs[start].oldNo
	newStart := recs[start].newNo
	if oldCount == 0 {
		oldStart--
	}
	if newCount == 0 {
		newStart--
	}
	fmt.Fprintf(b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for _, r := range recs[start:end] {
		switch r.kind {
		case '=':
			b.WriteString(" " + r.text + "\n")
		case '-':
			b.WriteString("-" + r.text + "\n")
		case '+':
			b.WriteString("+" + r.text + "\n")
		}
	}
}
