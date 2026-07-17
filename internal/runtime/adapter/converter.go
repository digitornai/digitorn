package adapter

import (
	"context"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

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
