package filesystem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

var skipDirs = map[string]struct{}{
	".git": {}, ".hg": {}, ".svn": {}, ".bzr": {},
	"node_modules": {}, ".venv": {}, "venv": {}, "__pycache__": {},
	".mypy_cache": {}, ".pytest_cache": {}, ".ruff_cache": {}, ".tox": {},
	"dist": {}, "build": {}, "target": {}, ".next": {}, ".nuxt": {},
	".idea": {}, ".vscode": {}, ".cache": {}, "vendor": {},
	".svelte-kit": {}, ".turbo": {}, ".parcel-cache": {}, ".angular": {},
	".astro": {}, ".vercel": {}, ".netlify": {}, ".expo": {}, ".docusaurus": {},
	"bower_components": {}, "coverage": {}, ".nyc_output": {},
	".gradle": {}, ".terraform": {}, ".serverless": {}, ".dart_tool": {},
	".pub-cache": {}, "Pods": {}, ".bundle": {},
}

// isSkipped reports whether a directory should be skipped during a walk.
// It excludes VCS/build noise by name AND the shadow git repo (.digitorn/git)
// by path — but NOT the rest of .digitorn/ (memory, config, etc. are useful
// to the agent).
func isSkipped(name, absPath string) bool {
	if _, ok := skipDirs[name]; ok {
		return true
	}
	// Skip only the shadow git repo inside .digitorn, not the whole directory.
	if name == "git" {
		parent := filepath.Base(filepath.Dir(absPath))
		if parent == ".digitorn" {
			return true
		}
	}
	return false
}

// IsNoiseDir reports whether a directory name is VCS/build/dependency noise.
// Exported so the daemon's workspace-tree route shares this single source of
// truth. For path-aware checks use isSkipped instead.
func IsNoiseDir(name string) bool {
	_, ok := skipDirs[name]
	return ok
}

func isProtectedFile(absPath string) bool {
	parent := filepath.Base(filepath.Dir(absPath))
	base := filepath.Base(absPath)
	return parent == ".digitorn" && base == "settings.yaml"
}

// grepMatch is one content-mode hit. Context lines (when requested) ride in
// Before/After so the agent sees the surrounding code.
type grepMatch struct {
	File    string   `json:"file"`
	LineNum int      `json:"line"`
	Ref     string   `json:"ref"`             // "file:line" — copy-paste into edit(start_line=N)
	Text    string   `json:"text"`
	Before  []string `json:"before,omitempty"`
	After   []string `json:"after,omitempty"`
}

type grepOutput string

const (
	grepContent grepOutput = "content"
	grepFiles   grepOutput = "files_with_matches"
	grepCount   grepOutput = "count"
)

type grepRequest struct {
	root        string
	base        string                          // workspace/globBase, for gitignore-relative paths
	rel         func(abs string) (string, bool) // abs → workspace-relative, ok=inside
	confine     func(abs string) bool           // symlink-safe membership (PathPolicy)
	ignore      *ignoreRules                    // .gitignore filter (nil = ignore nothing)
	re          *regexp.Regexp                  // compiled pattern (nil if pure literal)
	literal     []byte                          // non-nil → literal fast path (no regexp)
	include     string                          // optional basename glob filter
	includeAlts []string                        // brace-expanded include ("*.{ts,tsx}" → *.ts, *.tsx)
	mode        grepOutput
	contextN    int
	maxResults  int
	maxFileSize int64
}

type grepResult struct {
	Matches   []grepMatch
	Files     []string
	Count     int
	Truncated bool
	Scanned   int // files actually scanned (observability)
}

// fileHits is one file's contribution from a worker.
type fileHits struct {
	file    string
	matches []grepMatch
	count   int
}

// bufPool recycles per-file read buffers so a deep search doesn't churn the GC.
var bufPool = sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}

// fileEnumerator yields candidate absolute paths to scan. It must stop calling
// yield once yield returns false (ctx cancelled or result cap reached). runGrep
// is driven by one of two enumerators : walkEnum (full confined tree) or listEnum
// (trigram-index candidates ∪ dirty files).
type fileEnumerator func(yield func(path string) bool)

// acceptFile applies the basename include filter and the symlink-safe
// confinement check shared by both enumerators.
func acceptFile(req grepRequest, path, name string) bool {
	if req.include != "" {
		matched := false
		for _, pat := range req.includeAlts {
			if ok, _ := filepath.Match(pat, name); ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if req.ignore != nil {
		if rel, ok := relUnder(req.base, path); ok && req.ignore.ignored(rel, false) {
			return false // excluded by .gitignore
		}
	}
	if req.confine != nil && !req.confine(path) {
		return false // resolves outside the workdir (symlink escape)
	}
	return true
}

// walkEnum enumerates the confined tree, skipping VCS/build noise dirs and
// non-regular files. This is the full-scan path used when no usable index exists.
func walkEnum(req grepRequest) fileEnumerator {
	return func(yield func(string) bool) {
		_ = filepath.WalkDir(req.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entry : skip, never abort the whole walk
			}
			if d.IsDir() {
				if isSkipped(d.Name(), p) && p != req.root {
					return filepath.SkipDir
				}
				if req.ignore != nil && p != req.root {
					if rel, ok := relUnder(req.base, p); ok && req.ignore.ignored(rel, true) {
						return filepath.SkipDir // .gitignore'd directory : prune the subtree
					}
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if isProtectedFile(p) {
				return nil
			}
			if !acceptFile(req, p, d.Name()) {
				return nil
			}
			if !yield(p) {
				return filepath.SkipAll
			}
			return nil
		})
	}
}

// listEnum enumerates an explicit candidate list (trigram-index hits ∪ dirty
// files), confined + include-filtered exactly like the walk path. Stale entries
// (files deleted since indexing) are silently skipped by the scanner on open.
func listEnum(req grepRequest, files []string) fileEnumerator {
	return func(yield func(string) bool) {
		for _, p := range files {
			if !acceptFile(req, p, filepath.Base(p)) {
				continue
			}
			if !yield(p) {
				return
			}
		}
	}
}

// runGrep executes the parallel search over the files produced by enum. It
// returns when enumeration is exhausted, the result cap is hit, or ctx is
// cancelled (whichever comes first).
func runGrep(ctx context.Context, req grepRequest, enum fileEnumerator) (grepResult, error) {
	// Scanning is I/O-bound (open+read each file), so a worker blocks on the
	// kernel far more than it burns CPU. Oversubscribing past GOMAXPROCS keeps
	// more I/O requests in flight — a large throughput win on platforms with
	// high per-open latency (Windows + AV scanning). Capped so a pathological
	// tree can't spawn an unbounded fan-out.
	workers := runtime.GOMAXPROCS(0) * 4
	if workers < 4 {
		workers = 4
	}
	if workers > 64 {
		workers = 64
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	paths := make(chan string, 256)
	out := make(chan fileHits, workers)
	var scanned int64
	var capped atomic.Bool

	// Producer : drive the enumerator, forwarding paths until cancelled/capped.
	go func() {
		defer close(paths)
		enum(func(p string) bool {
			if ctx.Err() != nil || capped.Load() {
				return false
			}
			select {
			case paths <- p:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()

	// Workers : scan files in parallel.
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for p := range paths {
				if ctx.Err() != nil || capped.Load() {
					return
				}
				atomic.AddInt64(&scanned, 1)
				fh := scanFile(p, req)
				if fh.count == 0 && len(fh.matches) == 0 {
					continue
				}
				rel, ok := req.rel(p)
				if !ok {
					continue
				}
				fh.file = rel
				for i := range fh.matches {
					fh.matches[i].File = rel
					fh.matches[i].Ref = fmt.Sprintf("%s:%d", rel, fh.matches[i].LineNum)
				}
				select {
				case out <- fh:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(out) }()

	// Collector : gather, enforce the cap, then sort for determinism.
	res := grepResult{}
	total := 0
	for fh := range out {
		switch req.mode {
		case grepFiles:
			res.Files = append(res.Files, fh.file)
			total++
		case grepCount:
			res.Count += fh.count
			total += fh.count
		default:
			res.Matches = append(res.Matches, fh.matches...)
			total += len(fh.matches)
		}
		if req.maxResults > 0 && total >= req.maxResults && !capped.Load() {
			res.Truncated = true
			capped.Store(true)
			cancel() // stop producer + workers promptly
		}
	}
	res.Scanned = int(atomic.LoadInt64(&scanned))

	switch req.mode {
	case grepFiles:
		sort.Strings(res.Files)
		res.Files = dedupSorted(res.Files)
		if req.maxResults > 0 && len(res.Files) > req.maxResults {
			res.Files = res.Files[:req.maxResults]
		}
	case grepCount:
		// nothing to sort
	default:
		sort.SliceStable(res.Matches, func(a, b int) bool {
			if res.Matches[a].File != res.Matches[b].File {
				return res.Matches[a].File < res.Matches[b].File
			}
			return res.Matches[a].LineNum < res.Matches[b].LineNum
		})
		if req.maxResults > 0 && len(res.Matches) > req.maxResults {
			res.Matches = res.Matches[:req.maxResults]
		}
	}
	if err := ctx.Err(); err != nil && !res.Truncated {
		return res, err
	}
	return res, nil
}

// scanFile reads one file (bounded), skips binaries, and finds matches using a
// pooled buffer. Per-line scanning is the default ; multiline patterns scan the
// whole buffer and report each match's starting line.
func scanFile(path string, req grepRequest) fileHits {
	var fh fileHits
	f, err := os.Open(path)
	if err != nil {
		return fh
	}
	defer f.Close()

	limit := req.maxFileSize
	if limit <= 0 {
		limit = 10 << 20
	}
	// Own the pooled buffer for the whole scan : every match copies its text out
	// via string(...), so buf is never retained past this call — safe to recycle
	// at return with zero per-file allocation and no cross-worker race.
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf, ok := readInto(bp, f, limit)
	if !ok || isBinary(buf) {
		return fh
	}

	// files-with-matches : only existence matters — one optimized whole-buffer
	// scan, no line splitting, no per-match work.
	if req.mode == grepFiles {
		if req.matchesBuf(buf) {
			fh.count = 1
		}
		return fh
	}

	// content/count : find every match's START offset over the WHOLE buffer
	// (bytes.Index for literals — SIMD-accelerated in the stdlib — or the regexp
	// engine), then resolve line numbers ONLY for matches. A non-matching file
	// (the common case in a big tree) splits nothing and allocates nothing.
	offsets := req.matchOffsets(buf)
	if len(offsets) == 0 {
		return fh
	}
	lines := splitKeep(buf)
	starts := lineStartOffsets(lines)
	lastLine := -1
	for _, off := range offsets {
		ln := lineOf(starts, off)
		if ln == lastLine {
			continue // one result per matching line (grep semantics)
		}
		lastLine = ln
		if req.mode == grepCount {
			fh.count++
			continue
		}
		fh.matches = append(fh.matches, contentMatch(lines, ln, req.contextN))
	}
	return fh
}

// matchesBuf reports whether buf contains any match (existence only).
func (req grepRequest) matchesBuf(buf []byte) bool {
	if req.literal != nil {
		return bytes.Contains(buf, req.literal)
	}
	return req.re.Match(buf)
}

// matchOffsets returns the start byte offset of every (non-overlapping) match in
// buf, in ascending order. The regexp path uses (?m) so ^/$ anchor per line.
func (req grepRequest) matchOffsets(buf []byte) []int {
	if req.literal != nil {
		var offs []int
		for off := 0; off < len(buf); {
			i := bytes.Index(buf[off:], req.literal)
			if i < 0 {
				break
			}
			offs = append(offs, off+i)
			off += i + len(req.literal)
		}
		return offs
	}
	locs := req.re.FindAllIndex(buf, -1)
	offs := make([]int, len(locs))
	for i, loc := range locs {
		offs[i] = loc[0]
	}
	return offs
}

func contentMatch(lines [][]byte, i, contextN int) grepMatch {
	gm := grepMatch{LineNum: i + 1, Text: safeLine(lines[i])}
	if contextN > 0 {
		lo := i - contextN
		if lo < 0 {
			lo = 0
		}
		for k := lo; k < i; k++ {
			gm.Before = append(gm.Before, safeLine(lines[k]))
		}
		hi := i + contextN
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for k := i + 1; k <= hi; k++ {
			gm.After = append(gm.After, safeLine(lines[k]))
		}
	}
	return gm
}

// maxMatchLineBytes bounds the text returned for a single matched (or context)
// line. A match in a minified / generated file can be ONE multi-megabyte
// "line"; returning it whole bloats the result, the LLM context, and can crash
// a terminal client trying to render it.
// maxMatchLineBytes caps one matched line so a minified / generated megabyte
// "line" can't bloat the result — but it must be generous enough to show a real
// code line in full (a long signature, a wrapped call, a string/JSON literal).
// Aligned with read's per-line budget so grep and read clip at the same point.
const maxMatchLineBytes = 2000

// safeLine makes one matched line safe to return AND to display anywhere : it
// truncates before any decoding (so a 5 MB line costs only that many bytes of work) and
// neutralises control bytes (ESC / NUL / CR / …) that would otherwise corrupt or
// crash a terminal. Tabs survive; everything sub-space becomes a space. A grep
// result must never be able to take down the client that renders it.
func safeLine(raw []byte) string {
	b := trimEOL(raw)
	truncated := false
	if len(b) > maxMatchLineBytes {
		b = b[:maxMatchLineBytes]
		truncated = true
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for _, r := range string(b) {
		switch {
		case r == '\t':
			sb.WriteByte('\t')
		case r == utf8.RuneError || unicode.IsControl(r):
			sb.WriteByte(' ')
		default:
			sb.WriteRune(r)
		}
	}
	s := sb.String()
	if truncated {
		s += " …[line truncated]"
	}
	return s
}

// readInto reads up to limit bytes into the caller's pooled buffer (growing it
// in place and storing the grown slice back so the pool keeps the larger
// capacity). The returned slice aliases the pooled buffer — the caller owns it
// until it returns the buffer to the pool. ok=false when the file exceeds the
// limit (skipped rather than half-scanned) or on a read error.
func readInto(bp *[]byte, f *os.File, limit int64) ([]byte, bool) {
	buf := (*bp)[:0]
	r := io.LimitReader(f, limit+1)
	for {
		if len(buf) == cap(buf) {
			grown := make([]byte, len(buf), max2(cap(buf)*2, 64*1024))
			copy(grown, buf)
			buf = grown
		}
		n, e := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if e == io.EOF {
			break
		}
		if e != nil {
			*bp = buf
			return nil, false
		}
	}
	*bp = buf // keep the (possibly grown) buffer in the pool for reuse
	if int64(len(buf)) > limit {
		return nil, false
	}
	return buf, true
}

// isBinary reports whether buf looks binary (a NUL byte in the first 8 KiB —
// the git/ripgrep heuristic). Cheap and reliable for source trees.
func isBinary(buf []byte) bool {
	n := len(buf)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// splitKeep splits buf into lines keeping the trailing newline on each, so line
// numbers and context are exact. The final line (no trailing newline) is kept.
func splitKeep(buf []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] == '\n' {
			out = append(out, buf[start:i+1])
			start = i + 1
		}
	}
	if start < len(buf) {
		out = append(out, buf[start:])
	}
	return out
}

func lineStartOffsets(lines [][]byte) []int {
	starts := make([]int, len(lines))
	off := 0
	for i, l := range lines {
		starts[i] = off
		off += len(l)
	}
	return starts
}

// lineOf returns the 0-based index of the line containing byte offset via binary
// search over precomputed line starts.
func lineOf(starts []int, off int) int {
	i := sort.Search(len(starts), func(i int) bool { return starts[i] > off })
	if i == 0 {
		return 0
	}
	return i - 1
}

func trimEOL(b []byte) []byte { return bytes.TrimRight(b, "\r\n") }

func dedupSorted(s []string) []string {
	if len(s) < 2 {
		return s
	}
	w := 1
	for i := 1; i < len(s); i++ {
		if s[i] != s[w-1] {
			s[w] = s[i]
			w++
		}
	}
	return s[:w]
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// compilePattern turns a user pattern + multiline flag into a matcher: a literal
// fast path when the pattern has no regex metacharacters (scanned with
// bytes.Index), else a compiled regexp. The regexp always carries (?m) so ^/$
// anchor per line over the whole-buffer scan (grep semantics) ; multiline adds
// (?s) so "." spans line boundaries.
func compilePattern(pattern string, multiline bool) (re *regexp.Regexp, literal []byte, err error) {
	if !multiline && regexp.QuoteMeta(pattern) == pattern {
		return nil, []byte(pattern), nil
	}
	flags := "m"
	if multiline {
		flags += "s"
	}
	re, err = regexp.Compile("(?" + flags + ")" + pattern)
	return re, nil, err
}
