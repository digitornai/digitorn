package contextcompact

import "github.com/digitornai/digitorn/internal/runtime/sessionstore"

// estMsgTokens estimates ONE message's token cost with the documented
// chars/token heuristic. This estimate is used ONLY to decide the truncation
// cut — which recent messages to keep. The reported context occupancy is always
// the EXACT tokenizer count, never this estimate.
// safetyCharsPerToken is a CONSERVATIVE chars/token (3, vs the ~4 average) for
// the truncation cut decision : code/JSON tokenizes denser than prose, so a
// chars/4 budget under-counts and keeps too much. Over-counting here drops a
// touch more — the safe direction for holding the window. The reported occupancy
// is always the exact tokenizer count, never this.
const safetyCharsPerToken = 3

func estMsgTokens(m sessionstore.Message) int {
	return (len(messageText(m)) + len(m.Role) + 4) / safetyCharsPerToken
}

// SafeSplitIndexBudget is the token-budget truncation cut. It keeps the most
// recent messages that fit in tokenBudget — so a handful of HUGE tool results
// can't hold the whole window hostage (which a fixed keep_recent COUNT can't
// prevent). Guarantees, in order of precedence:
//
//   - tool-pair safety ALWAYS wins : the cut is pulled earlier until the kept
//     slice has no orphan tool result, even if that exceeds the budget.
//   - it drops AT LEAST as much as the count-based SafeSplitIndex(keepRecent).
//   - it keeps AT LEAST the single most-recent message (the engine snips it if
//     that one message alone still overflows).
//
// tokenBudget <= 0 falls back to the plain count-based split.
func SafeSplitIndexBudget(msgs []sessionstore.Message, keepRecent, tokenBudget int) int {
	base := SafeSplitIndex(msgs, keepRecent)
	if tokenBudget <= 0 {
		return base
	}
	n := len(msgs)
	if n == 0 {
		return 0
	}
	// Walk the kept suffix newest→oldest, accumulating estimated tokens. The cut
	// is the first (oldest) message that would push the kept slice over budget.
	acc := 0
	cut := 0
	for i := n - 1; i >= 0; i-- {
		t := estMsgTokens(msgs[i])
		if acc+t > tokenBudget && i < n-1 {
			cut = i + 1
			break
		}
		acc += t
	}
	// Drop at least as much as the count-based split would.
	if base > cut {
		cut = base
	}
	// Tool-pair safety beats the budget : never strand a kept tool result whose
	// call was dropped.
	for cut > 0 && hasOrphanToolResult(msgs[cut:]) {
		cut--
	}
	return cut
}

// TruncateBudget is Truncate with a token budget on the kept conversation : the
// never-fails path that actually holds the window even when recent messages are
// individually large. Same recap + cutoff contract as Truncate.
func TruncateBudget(msgs []sessionstore.Message, keepRecent, tokenBudget int, goal string) Result {
	cut := SafeSplitIndexBudget(msgs, keepRecent, tokenBudget)
	if cut == 0 {
		return Result{Messages: msgs, Dropped: 0, Strategy: StrategyTruncate}
	}
	recap := BuildReminder(msgs[:cut], goal)
	return Result{
		Messages:  prependSystem(recap, msgs[cut:]),
		Dropped:   cut,
		CutoffSeq: msgs[cut-1].Seq,
		Summary:   recap,
		Strategy:  StrategyTruncate,
	}
}
