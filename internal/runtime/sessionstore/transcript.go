package sessionstore

import "sort"

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
