package runtime

import (
	"regexp"
	"strings"
)

var (
	// reThinkBlock matches a balanced inline chain-of-thought block —
	// <think>…</think> or <thinking>…</thinking>, case-insensitive, spanning
	// newlines. Open models (Kimi, Qwen, some DeepSeek routes) put reasoning here
	// in `content` instead of the structured reasoning_content field.
	reThinkBlock = regexp.MustCompile(`(?is)<think(?:ing)?\b[^>]*>(.*?)</think(?:ing)?\s*>`)
	// reThinkDangClose matches a closing tag with no surviving opener: the opener
	// was streamed as reasoning_content (or dropped) and only the close plus the
	// real answer remain — "…thinking…</think>the answer". The prefix is reasoning.
	reThinkDangClose = regexp.MustCompile(`(?is)^(.*?)</think(?:ing)?\s*>`)
	// reThinkDangOpen matches an opening tag with no close: generation was cut off
	// mid-thought — "<think>partial thinking…". Everything after the tag is reasoning.
	reThinkDangOpen = regexp.MustCompile(`(?is)<think(?:ing)?\b[^>]*>(.*)\z`)
)

// splitInlineReasoning separates an assistant message's user-facing answer from
// chain-of-thought a model emitted INLINE in content as <think>…</think>. It
// returns the cleaned answer and the extracted reasoning (empty when none was
// present). It is conservative: it only removes think-tagged spans, so a message
// with no think tags returns byte-identical (clean == content, reasoning == "").
//
// Why it exists: left in content, inline reasoning leaks to every channel AND is
// replayed into the next round's context — re-feeding the model its own raw
// thinking, which compounds confusion and tool-call loops. Lifting it to
// ReasoningContent keeps it (lossless, replayed structurally to reasoning models)
// without ever surfacing it as an answer.
func splitInlineReasoning(content string) (clean, reasoning string) {
	lo := strings.ToLower(content)
	if !strings.Contains(lo, "<think") && !strings.Contains(lo, "</think") {
		return content, ""
	}
	var think []string

	// 1) Pull out every balanced <think>…</think> block.
	clean = reThinkBlock.ReplaceAllStringFunc(content, func(m string) string {
		if sub := reThinkBlock.FindStringSubmatch(m); len(sub) > 1 {
			if t := strings.TrimSpace(sub[1]); t != "" {
				think = append(think, t)
			}
		}
		return ""
	})

	// 2) A leftover closing tag → everything before it was the (open-less) reasoning.
	if loc := reThinkDangClose.FindStringSubmatchIndex(clean); loc != nil {
		if t := strings.TrimSpace(clean[loc[2]:loc[3]]); t != "" {
			think = append(think, t)
		}
		clean = clean[loc[1]:] // drop the prefix and the close tag
	}

	// 3) A leftover opening tag with no close → truncated thought to the end.
	if loc := reThinkDangOpen.FindStringSubmatchIndex(clean); loc != nil {
		if t := strings.TrimSpace(clean[loc[2]:loc[3]]); t != "" {
			think = append(think, t)
		}
		clean = clean[:loc[0]]
	}

	return strings.TrimSpace(clean), strings.TrimSpace(strings.Join(think, "\n"))
}
