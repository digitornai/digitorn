package voice

import "strings"

type Segmenter struct {
	buf      []rune
	MinChars int
	MaxChars int
}

func NewSegmenter() *Segmenter { return &Segmenter{MinChars: 2, MaxChars: 220} }

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

func (s *Segmenter) Flush() string {
	c := strings.TrimSpace(string(s.buf))
	s.buf = s.buf[:0]
	return c
}

func (s *Segmenter) boundary() int {
	for i := 0; i < len(s.buf)-1; i++ {
		if isClauseEnd(s.buf[i]) && isSpace(s.buf[i+1]) && i+1 >= s.MinChars {
			return i + 1
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
