package runtime

import (
	"fmt"

	"github.com/mbathepaul/digitorn/internal/llm"
)

// maxMessageBytes caps a single message's text in the prompt. A message far
// larger than this (a 200 KB tool-result dump) is clipped — head + tail kept
// with a marker — so one oversized message can't blow the context window on its
// own, the edge auto_compact/emergency can't fix by dropping OTHER messages.
// Generous on purpose: real content fits; only pathological dumps are trimmed.
const maxMessageBytes = 100 * 1024

// snipOversizedMessages clips, in place, any single message (or text part) whose
// text exceeds maxBytes — keeping the start and end around a truncation marker.
func snipOversizedMessages(msgs []llm.ChatMessage, maxBytes int) {
	if maxBytes <= 0 {
		return
	}
	for i := range msgs {
		if len(msgs[i].Content) > maxBytes {
			msgs[i].Content = snipText(msgs[i].Content, maxBytes)
		}
		for j := range msgs[i].Parts {
			if len(msgs[i].Parts[j].Text) > maxBytes {
				msgs[i].Parts[j].Text = snipText(msgs[i].Parts[j].Text, maxBytes)
			}
		}
	}
}

// snipText keeps the head and tail of s around a marker stating how much was
// dropped, so the model sees both ends of an oversized payload.
func snipText(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const markerRoom = 80
	head := (maxBytes - markerRoom) / 2
	if head < 0 {
		head = 0
	}
	tail := maxBytes - markerRoom - head
	if tail < 0 {
		tail = 0
	}
	dropped := len(s) - head - tail
	return s[:head] +
		fmt.Sprintf("\n…[%d bytes truncated — oversized message clipped]…\n", dropped) +
		s[len(s)-tail:]
}
