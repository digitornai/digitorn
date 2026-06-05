package index

import (
	"strings"
	"unicode"
)

// stopwords are grammatical filler words dropped from both the index
// and the query. They carry no discriminative signal and, left in,
// match nearly every tool's description — drowning the real keyword
// and semantic signal once a catalog has more than a handful of
// tools. The list is deliberately limited to function words (articles,
// pronouns, prepositions, auxiliaries) in EN + FR ; it never contains
// tool verbs like get/set/read/list/run, so real tool tokens survive.
var stopwords = func() map[string]struct{} {
	words := []string{
		// English
		"the", "a", "an", "and", "or", "of", "to", "for", "in", "on", "at",
		"by", "with", "from", "as", "is", "are", "be", "was", "were", "been",
		"this", "that", "these", "those", "it", "its", "into", "than", "then",
		"but", "so", "if", "my", "your", "our", "their", "his", "her", "will",
		"would", "can", "could", "do", "does", "did", "has", "have", "had",
		"not", "no", "i", "you", "he", "she", "we", "they", "me", "us", "them",
		"him", "about", "over", "per", "via", "out", "up",
		// French
		"le", "la", "les", "un", "une", "des", "du", "de", "et", "ou", "au",
		"aux", "ce", "cet", "cette", "ces", "mon", "ma", "mes", "ton", "ta",
		"tes", "son", "sa", "ses", "ne", "pas", "que", "qui", "dans", "sur",
		"pour", "par", "avec", "sans", "est", "sont", "je", "tu", "il", "elle",
		"nous", "vous", "ils", "elles", "se", "ses", "leur", "leurs",
	}
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}()

func isStopword(tok string) bool {
	_, ok := stopwords[tok]
	return ok
}

// tokenizeWithCamel is the richer variant that preserves the
// camelCase break before lower-casing. Used by the builder where
// description text often comes in camelCase from JS-style schemas.
func tokenizeWithCamel(s string) []string {
	if s == "" {
		return nil
	}
	// Walk the runes, emitting tokens at non-letter boundaries AND
	// at lowercase→uppercase transitions.
	var out []string
	seen := make(map[string]struct{})
	add := func(tok string) {
		if len(tok) < 2 {
			return
		}
		lower := strings.ToLower(tok)
		if isStopword(lower) {
			return
		}
		if _, exists := seen[lower]; exists {
			return
		}
		seen[lower] = struct{}{}
		out = append(out, lower)
	}

	var buf strings.Builder
	prevLower := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if prevLower && unicode.IsUpper(r) {
				// camelCase boundary : "readFile" → "read" + "File"
				if buf.Len() > 0 {
					add(buf.String())
					buf.Reset()
				}
			}
			buf.WriteRune(r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
		default:
			if buf.Len() > 0 {
				add(buf.String())
				buf.Reset()
			}
			prevLower = false
		}
	}
	if buf.Len() > 0 {
		add(buf.String())
	}
	return out
}
