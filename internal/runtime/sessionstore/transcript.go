package sessionstore

import "sort"

// ReadTranscript reconstructs the COMPLETE, lossless message transcript of a
// session straight from disk — the storage snapshot's messages (everything up
// to its cutoff) followed by the message events appended since. It deliberately
// ignores LLM-context compaction: no cutoff trim, no summary injection. It is
// the source of truth for the human transcript (REST /history) and for the
// lossless snapshot rebuild, kept independent of the live in-memory state, whose
// message slice is bounded to the model's window.
//
// Sound because every message is written with AppendDurable (fsynced before it
// is projected in memory), so disk is never behind the in-memory view.
//
// Empty (not an error) when the session has no snapshot and no events.
func ReadTranscript(p Paths, sid string) ([]Message, error) {
	if sid == "" {
		return nil, nil
	}
	dir := p.SessionDir(sid)
	snap, _, err := ReadSnapshot(dir)
	if err != nil {
		return nil, err
	}
	jres, jerr := ReadJSONL(p.EventsFile(sid), JSONLBestEffort, "")
	return TranscriptFromParts(snap, jres.Events), jerr
}

// MessagesAfterCutoff returns the messages that survive an LLM-context
// compaction at cutoff — those with Seq > cutoff, plus any unsequenced (Seq 0,
// freshly appended) ones, matching contextcompact.ApplyView's keep rule. It
// ALLOCATES a fresh slice when it trims, so the large pre-cutoff backing array
// is released (bounding memory) rather than retained. cutoff 0 is a no-op.
func MessagesAfterCutoff(msgs []Message, cutoff uint64) []Message {
	if cutoff == 0 {
		return msgs
	}
	kept := 0
	for i := range msgs {
		if msgs[i].Seq == 0 || msgs[i].Seq > cutoff {
			kept++
		}
	}
	if kept == len(msgs) {
		return msgs
	}
	out := make([]Message, 0, kept)
	for i := range msgs {
		if msgs[i].Seq == 0 || msgs[i].Seq > cutoff {
			out = append(out, msgs[i])
		}
	}
	return out
}

// MergeMessagesBySeq returns the lossless union of two message lists, deduped
// by Seq and sorted ascending. Used where neither source is guaranteed complete
// on its own — e.g. the storage snapshot, which must hold the full transcript
// even if disk lags the bounded in-memory window (or vice versa). Seq-0
// (unsequenced) messages can't be deduped, so the first list's are kept and the
// second's are dropped (the second list is always the disk rebuild, which only
// ever carries sequenced messages).
func MergeMessagesBySeq(a, b []Message) []Message {
	bySeq := make(map[uint64]Message, len(a)+len(b))
	var zero []Message
	for i := range a {
		if a[i].Seq == 0 {
			zero = append(zero, a[i])
			continue
		}
		if _, ok := bySeq[a[i].Seq]; !ok {
			bySeq[a[i].Seq] = a[i]
		}
	}
	for i := range b {
		if b[i].Seq == 0 {
			continue
		}
		if _, ok := bySeq[b[i].Seq]; !ok {
			bySeq[b[i].Seq] = b[i]
		}
	}
	out := make([]Message, 0, len(bySeq)+len(zero))
	for _, m := range bySeq {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return append(out, zero...)
}

// TranscriptFromParts builds the full message transcript from an optional
// storage snapshot and a slice of events — pure, no I/O. snap.Messages cover
// every message up to snap.CutoffSeq ; events contribute the message events with
// Seq beyond that cutoff (an untruncated JSONL may still carry <= cutoff ones,
// which are skipped to avoid duplicates). Non-message events are ignored.
func TranscriptFromParts(snap *SessionSnapshot, events []Event) []Message {
	var out []Message
	var cutoff uint64
	if snap != nil {
		out = append(out, snap.Messages...)
		cutoff = snap.CutoffSeq
	}
	for i := range events {
		ev := &events[i]
		if ev.Seq <= cutoff || ev.Message == nil {
			continue
		}
		switch ev.Type {
		case EventUserMessage, EventAssistantMessage, EventSystemMessage:
		default:
			continue
		}
		parts, content, toolIDs, atts := NormalizeMessageParts(ev.Message)
		out = append(out, Message{
			Seq:         ev.Seq,
			StepID:      ev.StepID,
			Role:        ev.Message.Role,
			Parts:       parts,
			Content:     content,
		Reasoning:   ev.Message.Reasoning,
		ReasoningStartedAt: ev.Message.ReasoningStartedAt,
		ReasoningEndedAt:   ev.Message.ReasoningEndedAt,
			TsUnixNano:  ev.TsUnixNano,
			ToolCallIDs: toolIDs,
			Attachments: atts,
		})
	}
	return out
}
