package rag

import (
	"strings"
	"unicode/utf8"
)

// Chunk is one indexed slice of a source document. Index is the ordinal
// position within the document — carried into the vector payload so a
// retrieved chunk can cite "chunk N of <source>".
type Chunk struct {
	Text  string
	Index int
}

// Chunking strategies, mirroring the Python RAG module's options.
const (
	StrategyRecursive = "recursive"
	StrategySentence  = "sentence"
	StrategyParagraph = "paragraph"
	StrategyFixed     = "fixed"
)

// Chunkize splits text per the strategy into chunks of about size runes
// with overlap runes of carry-over between neighbours. size/overlap fall
// back to the doc defaults (500/50) when non-positive. Returns one chunk
// per document when the text already fits.
func Chunkize(text, strategy string, size, overlap int) []Chunk {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if size <= 0 {
		size = 500
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 10
	}

	var pieces []string
	switch strategy {
	case StrategyFixed:
		pieces = fixedSplit(text, size, overlap)
	case StrategyParagraph:
		pieces = recursiveSplit(text, []string{"\n\n", "\n", " ", ""}, size, overlap)
	case StrategySentence:
		pieces = recursiveSplit(text, []string{". ", "! ", "? ", "\n", " ", ""}, size, overlap)
	default: // recursive
		pieces = recursiveSplit(text, []string{"\n\n", "\n", ". ", " ", ""}, size, overlap)
	}

	out := make([]Chunk, 0, len(pieces))
	for _, p := range pieces {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, Chunk{Text: p, Index: len(out)})
		}
	}
	return out
}

// recursiveSplit is the LangChain-style recursive character splitter : it
// tries separators from coarse to fine, splitting oversized pieces with
// the next separator, then packs small pieces up to size with overlap.
func recursiveSplit(text string, seps []string, size, overlap int) []string {
	var final []string

	sep := seps[len(seps)-1]
	rest := []string{}
	for i, s := range seps {
		if s == "" {
			sep = ""
			rest = nil
			break
		}
		if strings.Contains(text, s) {
			sep = s
			rest = seps[i+1:]
			break
		}
	}

	splits := splitKeep(text, sep)

	var good []string
	for _, s := range splits {
		if s == "" {
			continue
		}
		if runeLen(s) < size {
			good = append(good, s)
			continue
		}
		if len(good) > 0 {
			final = append(final, mergeSplits(good, size, overlap)...)
			good = nil
		}
		if len(rest) == 0 {
			final = append(final, hardWrap(s, size)...)
		} else {
			final = append(final, recursiveSplit(s, rest, size, overlap)...)
		}
	}
	if len(good) > 0 {
		final = append(final, mergeSplits(good, size, overlap)...)
	}
	return final
}

// splitKeep splits on sep, keeping sep appended to each piece so rejoined
// chunks read naturally. An empty sep splits into runes.
func splitKeep(text, sep string) []string {
	if sep == "" {
		out := make([]string, 0, runeLen(text))
		for _, r := range text {
			out = append(out, string(r))
		}
		return out
	}
	parts := strings.SplitAfter(text, sep)
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mergeSplits packs consecutive small pieces into chunks up to size,
// carrying overlap runes from the tail of one chunk into the next.
func mergeSplits(splits []string, size, overlap int) []string {
	var chunks []string
	var cur []string
	curLen := 0
	for _, s := range splits {
		sl := runeLen(s)
		if curLen+sl > size && len(cur) > 0 {
			chunks = append(chunks, strings.Join(cur, ""))
			// drop from the front until the carry-over fits the overlap budget.
			for curLen > overlap && len(cur) > 0 {
				curLen -= runeLen(cur[0])
				cur = cur[1:]
			}
		}
		cur = append(cur, s)
		curLen += sl
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, ""))
	}
	return chunks
}

// fixedSplit cuts text into fixed-size rune windows with overlap.
func fixedSplit(text string, size, overlap int) []string {
	runes := []rune(text)
	step := size - overlap
	if step <= 0 {
		step = size
	}
	var out []string
	for i := 0; i < len(runes); i += step {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}

// hardWrap is the last resort for a single token longer than size and not
// splittable by any separator : cut it into size-rune windows.
func hardWrap(s string, size int) []string {
	runes := []rune(s)
	var out []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
