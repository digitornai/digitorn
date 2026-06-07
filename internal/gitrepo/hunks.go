package gitrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v5"
)

// diffHunk is one hunk of a unified diff, identified by a stable 12-char hash
// computed IDENTICALLY to the web client (models/unified-diff-hunk.ts) and the
// legacy Python daemon:
//
//	hash = sha256(header + "\n" + body.join("\n")).hex()[:12]
//
// body keeps only the content lines (first char " ", "-", or "+"); the
// "--- a/…" / "+++ b/…" file markers never reach it (they precede the first
// @@). A client clicks a hunk by hash, so the daemon must hash byte-for-byte
// the same — otherwise the approve/reject-hunks call references nothing.
type diffHunk struct {
	Hash     string
	Header   string
	OldStart int
	OldLen   int
	NewStart int
	NewLen   int
	Body     []string
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func hunkHashOf(header string, body []string) string {
	sum := sha256.Sum256([]byte(header + "\n" + strings.Join(body, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// parseUnifiedHunks parses a unified diff (one file) into hunks with
// web-identical hashes. Lines before the first @@ (the diff/index/file-marker
// header) are ignored.
func parseUnifiedHunks(unified string) []diffHunk {
	var out []diffHunk
	var cur *diffHunk
	flush := func() {
		if cur == nil {
			return
		}
		cur.Hash = hunkHashOf(cur.Header, cur.Body)
		out = append(out, *cur)
		cur = nil
	}
	for _, line := range strings.Split(unified, "\n") {
		if strings.HasPrefix(line, "@@") {
			flush()
			m := hunkHeaderRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			h := diffHunk{Header: line, OldLen: 1, NewLen: 1}
			h.OldStart, _ = strconv.Atoi(m[1])
			if m[2] != "" {
				h.OldLen, _ = strconv.Atoi(m[2])
			}
			h.NewStart, _ = strconv.Atoi(m[3])
			if m[4] != "" {
				h.NewLen, _ = strconv.Atoi(m[4])
			}
			cur = &h
			continue
		}
		if cur != nil && len(line) > 0 && (line[0] == ' ' || line[0] == '-' || line[0] == '+') {
			cur.Body = append(cur.Body, line)
		}
	}
	flush()
	return out
}

// applyHunks applies a SUBSET of hunks (any order) to the baseline lines and
// returns the patched lines. Hunks are applied in ascending OldStart order.
// Context (' ') and deletion ('-') lines are verified against the baseline; on
// any mismatch it returns an error — a stale hunk is REFUSED, never applied
// blind, so a concurrent edit can never silently corrupt the file. Pure.
func applyHunks(baseline []string, hunks []diffHunk) ([]string, error) {
	sorted := append([]diffHunk(nil), hunks...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].OldStart < sorted[j].OldStart })

	out := make([]string, 0, len(baseline))
	cursor := 0 // 0-based index into baseline; next not-yet-emitted line
	for _, h := range sorted {
		start := h.OldStart - 1
		if h.OldLen == 0 {
			start = h.OldStart // pure insertion: insert AFTER line OldStart
		}
		if start < 0 {
			start = 0
		}
		if start < cursor {
			return nil, fmt.Errorf("hunks overlap or out of order near @@ -%d", h.OldStart)
		}
		if start > len(baseline) {
			return nil, fmt.Errorf("hunk start %d is beyond the file (%d lines)", h.OldStart, len(baseline))
		}
		out = append(out, baseline[cursor:start]...)
		bi := start
		for _, bl := range h.Body {
			switch bl[0] {
			case ' ':
				if bi >= len(baseline) || baseline[bi] != bl[1:] {
					return nil, fmt.Errorf("stale hunk: context mismatch at line %d", bi+1)
				}
				out = append(out, baseline[bi])
				bi++
			case '-':
				if bi >= len(baseline) || baseline[bi] != bl[1:] {
					return nil, fmt.Errorf("stale hunk: deletion mismatch at line %d", bi+1)
				}
				bi++
			case '+':
				out = append(out, bl[1:])
			}
		}
		cursor = bi
	}
	out = append(out, baseline[cursor:]...)
	return out, nil
}

// splitLines splits file content into lines, recording whether it ended with a
// trailing newline so joinLines can round-trip it. "" → (nil,false).
func splitLines(content string) (lines []string, trailingNewline bool) {
	if content == "" {
		return nil, false
	}
	trailingNewline = strings.HasSuffix(content, "\n")
	if trailingNewline {
		content = content[:len(content)-1]
	}
	return strings.Split(content, "\n"), trailingNewline
}

// joinLines is the inverse of splitLines.
func joinLines(lines []string, trailingNewline bool) string {
	s := strings.Join(lines, "\n")
	if trailingNewline {
		s += "\n"
	}
	return s
}

// selectHunks returns the hunks whose hash is in want, and the rest, preserving
// order. Unknown hashes in want are reported.
func selectHunks(all []diffHunk, want []string) (selected, others []diffHunk, missing []string) {
	wantSet := make(map[string]bool, len(want))
	for _, h := range want {
		wantSet[h] = true
	}
	seen := make(map[string]bool, len(all))
	for _, h := range all {
		seen[h.Hash] = true
		if wantSet[h.Hash] {
			selected = append(selected, h)
		} else {
			others = append(others, h)
		}
	}
	for _, h := range want {
		if !seen[h] {
			missing = append(missing, h)
		}
	}
	return selected, others, missing
}

// ApproveHunks commits ONLY the selected hunks of a file (baseline + selected),
// leaving the remaining hunks pending in the worktree — the "stage this hunk"
// action. The diff is recomputed from the same FileDiff source the client saw,
// so the per-hunk hashes match byte-for-byte; a hash that no longer exists (a
// concurrent edit moved/removed the hunk) is reported, never applied blind.
func (r *Repo) ApproveHunks(path string, hashes []string, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rel := filepath.ToSlash(path)
	unified, oldContent, newContent, _, _, err := r.fileDiffLocked(rel)
	if err != nil {
		return err
	}
	hunks := parseUnifiedHunks(unified)
	selected, _, missing := selectHunks(hunks, hashes)
	if len(missing) > 0 {
		return fmt.Errorf("hunk(s) not found (stale diff — refresh): %s", strings.Join(missing, ", "))
	}
	if len(selected) == 0 {
		return fmt.Errorf("no matching hunks to approve")
	}
	baseLines, tnl := splitLines(oldContent)
	approvedLines, err := applyHunks(baseLines, selected)
	if err != nil {
		return err
	}
	approved := joinLines(approvedLines, tnl)
	abs := filepath.Join(r.workdir, filepath.FromSlash(rel))

	// Worktree dance (mutex held): write baseline+selected, stage + commit ONLY
	// this path, then restore the full current content so the unselected hunks
	// stay pending. Any failure restores the full content.
	if err := os.WriteFile(abs, []byte(approved), 0o644); err != nil {
		return err
	}
	if err := r.wt.AddWithOptions(&git.AddOptions{Path: rel}); err != nil {
		_ = os.WriteFile(abs, []byte(newContent), 0o644)
		return err
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = fmt.Sprintf("Approve %d hunk(s) in %s", len(selected), rel)
	}
	if _, err := r.commit(msg); err != nil {
		_ = os.WriteFile(abs, []byte(newContent), 0o644)
		return err
	}
	return os.WriteFile(abs, []byte(newContent), 0o644)
}

// RejectHunks reverts ONLY the selected hunks in the worktree — the file becomes
// baseline + the UNselected hunks, discarding the selected changes. No commit
// (the "revert this hunk" action). Rejecting every hunk reverts the whole file.
func (r *Repo) RejectHunks(path string, hashes []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rel := filepath.ToSlash(path)
	unified, oldContent, _, _, _, err := r.fileDiffLocked(rel)
	if err != nil {
		return err
	}
	hunks := parseUnifiedHunks(unified)
	selected, others, missing := selectHunks(hunks, hashes)
	if len(missing) > 0 {
		return fmt.Errorf("hunk(s) not found (stale diff — refresh): %s", strings.Join(missing, ", "))
	}
	if len(selected) == 0 {
		return fmt.Errorf("no matching hunks to reject")
	}
	baseLines, tnl := splitLines(oldContent)
	keptLines, err := applyHunks(baseLines, others)
	if err != nil {
		return err
	}
	abs := filepath.Join(r.workdir, filepath.FromSlash(rel))
	return os.WriteFile(abs, []byte(joinLines(keptLines, tnl)), 0o644)
}
