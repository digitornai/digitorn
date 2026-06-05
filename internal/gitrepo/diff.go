package gitrepo

import (
	"bytes"

	"github.com/go-git/go-git/v5/plumbing"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// encodeUnified renders a git-style unified diff for one file from the
// line-oriented diff of its old (HEAD) and new (working-tree) contents.
func encodeUnified(path, oldContent, newContent string, diffs []diffmatchpatch.Diff) (string, error) {
	if oldContent == newContent {
		return "", nil
	}
	chunks := make([]fdiff.Chunk, 0, len(diffs))
	for _, d := range diffs {
		if d.Text == "" {
			continue
		}
		var op fdiff.Operation
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			op = fdiff.Equal
		case diffmatchpatch.DiffInsert:
			op = fdiff.Add
		case diffmatchpatch.DiffDelete:
			op = fdiff.Delete
		}
		chunks = append(chunks, textChunk{content: d.Text, op: op})
	}

	p := textPatch{fps: []fdiff.FilePatch{textFilePatch{
		from:   textFile{path: path},
		to:     textFile{path: path},
		chunks: chunks,
	}}}

	var buf bytes.Buffer
	if err := fdiff.NewUnifiedEncoder(&buf, 3).Encode(p); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// The four tiny adapters below implement go-git's diff.Patch interfaces so we
// can reuse its UnifiedEncoder to format the output exactly like git does.

type textChunk struct {
	content string
	op      fdiff.Operation
}

func (c textChunk) Content() string       { return c.content }
func (c textChunk) Type() fdiff.Operation { return c.op }

type textFile struct{ path string }

func (f textFile) Hash() plumbing.Hash     { return plumbing.ZeroHash }
func (f textFile) Mode() filemode.FileMode { return filemode.Regular }
func (f textFile) Path() string            { return f.path }

type textFilePatch struct {
	from, to fdiff.File
	chunks   []fdiff.Chunk
}

func (f textFilePatch) IsBinary() bool                  { return false }
func (f textFilePatch) Files() (fdiff.File, fdiff.File) { return f.from, f.to }
func (f textFilePatch) Chunks() []fdiff.Chunk           { return f.chunks }

type textPatch struct{ fps []fdiff.FilePatch }

func (p textPatch) FilePatches() []fdiff.FilePatch { return p.fps }
func (p textPatch) Message() string                { return "" }
