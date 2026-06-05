package adapter

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// Converter is a stateful, incremental front-end to MessagesToLLM. Within
// one turn the history grows by a few messages per LLM iteration, yet the
// stateless MessagesToLLM re-converts — and re-loads every blob — on each
// call. Converter caches the per-message conversion keyed by event Seq
// (messages are immutable once appended), so each iteration only converts
// the new tail and never re-fetches a blob it already loaded.
//
// Output equals MessagesToLLM(history) exactly: the cache stores precisely
// convertOne(m), and the same repairToolPairing pass runs over the
// assembled list on every call. Messages with Seq==0 carry no stable
// identity and are converted fresh each time.
//
// Not safe for concurrent use; scope one Converter to one turn.
type Converter struct {
	opts  Options
	cache map[uint64][]llm.ChatMessage
}

func NewConverter(opts Options) *Converter {
	return &Converter{opts: opts, cache: make(map[uint64][]llm.ChatMessage)}
}

func (c *Converter) Convert(ctx context.Context, msgs []sessionstore.Message) []llm.ChatMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]llm.ChatMessage, 0, len(msgs))
	for i := range msgs {
		m := msgs[i]
		if m.Seq != 0 {
			if cached, ok := c.cache[m.Seq]; ok {
				out = append(out, cached...)
				continue
			}
		}
		conv := convertOne(ctx, m, c.opts)
		if m.Seq != 0 {
			c.cache[m.Seq] = conv
		}
		out = append(out, conv...)
	}
	return repairToolPairing(out, c.opts)
}
