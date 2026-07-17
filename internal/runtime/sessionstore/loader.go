package sessionstore

import (
	"fmt"
	"os"
	"time"
)

type LoadOptions struct {
	Mode JSONLReadMode
}

type LoadResult struct {
	State           *SessionState
	HadSnapshot     bool
	SnapshotFormat  SnapshotFormat
	SnapshotCutoff  uint64
	EventsApplied   int
	EventsTotal     int
	BadEventLines   int
	BadEventOffsets []int64
	MetaRebuilt     bool
}

func Load(p Paths, sid string, opts LoadOptions) (*LoadResult, error) {
	if sid == "" {
		return nil, fmt.Errorf("load: empty session id")
	}
	dir := p.SessionDir(sid)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return &LoadResult{State: NewSessionState(sid)}, nil
		}
		return nil, fmt.Errorf("load stat: %w", err)
	}

	state := NewSessionState(sid)
	res := &LoadResult{State: state}

	snap, snapFmt, err := ReadSnapshot(dir)
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	if snap != nil {
		res.HadSnapshot = true
		res.SnapshotFormat = snapFmt
		res.SnapshotCutoff = snap.CutoffSeq
		hydrateFromSnapshot(state, snap)
	}

	jsonlPath := p.EventsFile(sid)
	quarantinePath := p.QuarantineFile(sid)
	jres, err := ReadJSONL(jsonlPath, opts.Mode, quarantinePath)
	if err != nil {
		return res, fmt.Errorf("load jsonl: %w", err)
	}
	res.EventsTotal = jres.LinesRead
	res.BadEventLines = jres.BadLines
	res.BadEventOffsets = jres.BadOffsets
	if jres.Partial {
		state.Partial = true
	}

	state.mu.Lock()
	for i := range jres.Events {
		ev := &jres.Events[i]
		if ev.Seq <= res.SnapshotCutoff {
			continue
		}
		applyLocked(state, ev)
		res.EventsApplied++
	}
	now := int64(0)
	for i := range state.Children {
		if state.Children[i].Status == "running" {
			state.Children[i].Status = "interrupted"
			state.Children[i].CurrentTool = ""
			if now == 0 {
				now = time.Now().UnixNano()
			}
			state.Children[i].UpdatedAt = now
		}
	}
	for i := range state.BackgroundTasks {
		if state.BackgroundTasks[i].State == "running" {
			state.BackgroundTasks[i].State = "interrupted"
		}
	}
	state.mu.Unlock()

	meta, err := ReadMeta(p.MetaFile(sid))
	if err != nil {
		meta = nil
	}
	if meta == nil || meta.LastSeq < state.LastSeq || meta.EventCount != uint64(jres.LinesRead) ||
		(meta.Preview == "" && previewFromMessages(state.Messages) != "") {
		newMeta := &Meta{
			SessionID:      sid,
			AppID:          state.AppID,
			UserID:         state.UserID,
			FirstSeq:       state.FirstSeq,
			LastSeq:        state.LastSeq,
			EventCount:     uint64(jres.LinesRead),
			StartedAtNano:  state.StartedAtNano,
			UpdatedAtNano:  state.LastEventTsNano,
			SnapshotCutoff: res.SnapshotCutoff,
			Partial:        state.Partial,
			Title:          state.Title,
			Workspace:      state.Workspace,
			Workdir:        state.Workdir,
			Preview:        previewFromMessages(state.Messages),
		}
		if meta != nil {
			newMeta.SnapshotSHA256 = meta.SnapshotSHA256
			newMeta.SnapshotBinary = meta.SnapshotBinary
		}
		if err := WriteMetaAtomic(dir, newMeta, false); err == nil {
			res.MetaRebuilt = true
		}
	}

	return res, nil
}

func hydrateFromSnapshot(s *SessionState, snap *SessionSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AppID = snap.AppID
	s.UserID = snap.UserID
	s.StartedAtNano = snap.StartedAtNano
	s.EndedAtNano = snap.EndedAtNano
	s.FirstSeq = snap.FirstSeq
	s.LastSeq = snap.LastSeq
	s.Messages = append(s.Messages[:0], snap.Messages...)
	if len(snap.ToolCalls) > 0 {
		s.ToolCalls = cloneToolCalls(snap.ToolCalls)
	}
	if len(snap.Approvals) > 0 {
		s.Approvals = cloneApprovals(snap.Approvals)
	}
	if len(snap.Memory) > 0 {
		s.Memory = cloneStringMap(snap.Memory)
	}
	s.Facts = append(s.Facts[:0], snap.Facts...)
	s.AllowedSignatures = append(s.AllowedSignatures[:0], snap.AllowedSignatures...)
	s.Goal = snap.Goal
	s.LastUserMessage = snap.LastUserMessage
	if len(snap.WorkspaceFiles) > 0 {
		s.WorkspaceFiles = cloneFileStates(snap.WorkspaceFiles)
	}
	s.Todos = append(s.Todos[:0], snap.Todos...)
	s.Children = append(s.Children[:0], snap.Children...)
	s.BackgroundTasks = append(s.BackgroundTasks[:0], snap.BackgroundTasks...)
	if len(snap.Widgets) > 0 {
		s.Widgets = cloneWidgets(snap.Widgets)
	}
	if len(snap.Previews) > 0 {
		s.Previews = clonePreviews(snap.Previews)
	}
	s.Errors = append(s.Errors[:0], snap.Errors...)
	s.Compactions = append(s.Compactions[:0], snap.Compactions...)
	s.ContextCompaction = cloneContextCompaction(snap.ContextCompaction)
	s.PreparedSummary = clonePreparedSummary(snap.PreparedSummary)
	if s.ContextCompaction != nil {
		s.Messages = MessagesAfterCutoff(s.Messages, s.ContextCompaction.CutoffSeq)
	}
	s.ContextTokens = snap.ContextTokens
	s.ContextSystemTokens = snap.ContextSystemTokens
	s.ContextToolsTokens = snap.ContextToolsTokens
	s.ContextMessageTokens = snap.ContextMessageTokens
	s.ContextProviderTokens = snap.ContextProviderTokens
		s.TokensIn = snap.TokensIn
		s.TokensOut = snap.TokensOut
		s.EntryModelWindow = snap.EntryModelWindow
	s.ReasoningEffort = snap.ReasoningEffort
	s.EffortOverrides = snap.EffortOverrides
	s.UsdTotal = snap.UsdTotal
	s.Title = snap.Title
	s.Workspace = snap.Workspace
	s.Workdir = snap.Workdir
	s.EntryAgent = snap.EntryAgent
	s.ContextExtra = snap.ContextExtra
	s.ModelOverrides = snap.ModelOverrides
	s.ProviderOverrides = snap.ProviderOverrides
	s.OutputTokenOverrides = snap.OutputTokenOverrides
	s.TurnCount = snap.TurnCount
	s.Interrupted = snap.Interrupted
	s.ActiveMode = snap.ActiveMode
	s.Closed = snap.Closed
	s.EventCount = snap.EventCount
	s.BytesEst = snap.BytesEst
	s.Partial = snap.Partial
}
