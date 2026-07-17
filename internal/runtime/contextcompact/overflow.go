package contextcompact

import "strings"

var overflowMarkers = []string{
	"maximum context length",
	"context_length_exceeded",
	"context window",
	"context length",
	"reduce the length of the messages",
	"too many tokens",
	"token limit",
	"prompt is too long",
}

func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range overflowMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func EmergencyKeepRecent(keepRecent int) int {
	if keepRecent <= 0 {
		keepRecent = defaultKeepRecent
	}
	half := keepRecent / 2
	if half < 4 {
		return 4
	}
	return half
}
