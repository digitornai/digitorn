package contextcompact

import "github.com/digitornai/digitorn/internal/runtime/sessionstore"

const safetyCharsPerToken = 3

func estMsgTokens(m sessionstore.Message) int {
	return (len(messageText(m)) + len(m.Role) + 4) / safetyCharsPerToken
}

func SafeSplitIndexBudget(msgs []sessionstore.Message, keepRecent, tokenBudget int) int {
	base := SafeSplitIndex(msgs, keepRecent)
	if tokenBudget <= 0 {
		return base
	}
	n := len(msgs)
	if n == 0 {
		return 0
	}
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
	if base > cut {
		cut = base
	}
	for cut > 0 && hasOrphanToolResult(msgs[cut:]) {
		cut--
	}
	return cut
}

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
