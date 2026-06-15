package sessionstore

import (
	"fmt"
	"os"
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
	// Crash reconciliation : this is a COLD load (fresh process), so no agent
	// goroutine exists. Any child still "running" was orphaned by a daemon stop
	// mid-delegation — it never wrote its agent_result. Flip it to "interrupted"
	// so the resync view is honest (no eternal "running" zombies). If that agent
	// is somehow still alive (rare evict-then-reload while up), its real
	// agent_result later overwrites this through the normal projection.
	for i := range state.Children {
		if state.Children[i].Status == "running" {
			state.Children[i].Status = "interrupted"
		}
	}
	// Same reconciliation for background tasks : a task left "running" had its
	// goroutine die with the daemon, so on cold load it's an orphan, not a live
	// task. Flip it to "interrupted" so the resync view is honest.
	for i := range state.BackgroundTasks {
		if state.BackgroundTasks[i].State == "running" {
			state.BackgroundTasks[i].State = "interrupted"
		}
	}
	state.mu.Unlock()

	meta, err := ReadMeta(p.MetaFile(sid))
	if err != nil {
		// Treat corrupt meta as missing — we just rebuild it.
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
	// Guard on len (not != nil) so an empty map on disk — or a nil one from
	// the alloc-free Snapshot path — never overwrites the non-nil map that
	// NewSessionState seeded; the projection writes to these maps assuming
	// they are never nil.
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
	s.Goal = snap.Goal
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
	// The snapshot carries the FULL transcript (lossless), but the live in-memory
	// window must load bounded to the model's view : if a context compaction had
	// already happened, drop the pre-cutoff messages here. Post-snapshot
	// EventContextCompacted replays trim further via the projection. The full
	// transcript is rebuilt from disk on demand (ReadTranscript), never from this.
	if s.ContextCompaction != nil {
		s.Messages = MessagesAfterCutoff(s.Messages, s.ContextCompaction.CutoffSeq)
	}
	// CTX-7 : restore the persisted context-occupancy gauge + breakdown so a
	// cold-loaded session reports its LAST real context immediately (e.g. the CLI
	// footer shows ctx used/window on open, before any new turn). They are written
	// to the snapshot but were never read back ; post-snapshot EventContextTokens
	// replays refine them via the projection.
	s.ContextTokens = snap.ContextTokens
	s.ContextSystemTokens = snap.ContextSystemTokens
	s.ContextToolsTokens = snap.ContextToolsTokens
	s.ContextMessageTokens = snap.ContextMessageTokens
	s.ContextProviderTokens = snap.ContextProviderTokens
	s.TokensIn = snap.TokensIn
	s.TokensOut = snap.TokensOut
	s.UsdTotal = snap.UsdTotal
	s.Title = snap.Title
	s.Workspace = snap.Workspace
	s.Workdir = snap.Workdir
	s.EntryAgent = snap.EntryAgent
	s.ContextExtra = snap.ContextExtra
	s.ModelOverrides = snap.ModelOverrides
	s.TurnCount = snap.TurnCount
	s.Interrupted = snap.Interrupted
	s.ActiveMode = snap.ActiveMode
	s.Closed = snap.Closed
	s.EventCount = snap.EventCount
	s.BytesEst = snap.BytesEst
	s.Partial = snap.Partial
}
