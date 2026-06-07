package voice

import "strings"

// Segmenter turns a stream of reply tokens into speakable clauses so TTS can start
// before the full reply is generated (the clause-pipeline that hides LLM latency).
// A clause flushes at sentence-final punctuation followed by whitespace, or when it
// grows past MaxChars (so a long run never stalls the voice).
type Segmenter struct {
	buf      []rune
	MinChars int
	MaxChars int
}

// NewSegmenter returns a segmenter with sane voice defaults.
func NewSegmenter() *Segmenter { return &Segmenter{MinChars: 2, MaxChars: 220} }

// Push appends a token and returns any clauses that completed.
func (s *Segmenter) Push(tok string) []string {
	s.buf = append(s.buf, []rune(tok)...)
	var out []string
	for {
		cut := s.boundary()
		if cut < 0 {
			break
		}
		if c := strings.TrimSpace(string(s.buf[:cut])); c != "" {
			out = append(out, c)
		}
		s.buf = append([]rune(nil), s.buf[cut:]...)
	}
	return out
}

// Flush returns the buffered remainder (call at end of reply).
func (s *Segmenter) Flush() string {
	c := strings.TrimSpace(string(s.buf))
	s.buf = s.buf[:0]
	return c
}

// boundary returns the cut index of the next complete clause, or -1.
func (s *Segmenter) boundary() int {
	for i := 0; i < len(s.buf)-1; i++ {
		if isClauseEnd(s.buf[i]) && isSpace(s.buf[i+1]) && i+1 >= s.MinChars {
			return i + 1 // cut right after the punctuation; the space leads the remainder
		}
	}
	if len(s.buf) >= s.MaxChars {
		return s.MaxChars
	}
	return -1
}

func isClauseEnd(r rune) bool {
	switch r {
	case '.', '!', '?', ';', '\n':
		return true
	}
	return false
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }
