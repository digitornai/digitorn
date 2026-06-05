package contextcompact

import "strings"

// overflowMarkers are the provider phrasings that mean "the prompt exceeded the
// model's context window". Verbatim from docs-site language/06-context-management
// (emergency_compact / is_context_overflow). Lower-cased for matching.
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

// IsContextOverflow reports whether err is a provider context-overflow error —
// the model refusing the request because the input no longer fits its window.
// This is the trigger for emergency compaction: auto_compact should prevent it,
// but a single huge tool result can blow past the threshold in one step.
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

// EmergencyKeepRecent halves the configured keep_recent (floor 4) for an
// aggressive overflow recovery — drop more history than a normal compaction so
// the retried request comfortably fits.
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
