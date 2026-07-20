package sessionstore

import (
	"fmt"
	"strings"
	"time"
)

const maxKeyFacts = 200

func Apply(s *SessionState, ev *Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	applyLocked(s, ev)
}

func previewFromMessages(msgs []Message) string {
	for i := range msgs {
		if msgs[i].Role != "user" {
			continue
		}
		txt := msgs[i].Content
		if txt == "" {
			for _, p := range msgs[i].Parts {
				txt += p.Text
			}
		}
		return CapPreview(txt)
	}
	return ""
}

func applyLocked(s *SessionState, ev *Event) {
	if ev.Seq != 0 && ev.Seq <= s.LastSeq {
		return
	}
	if s.FirstSeq == 0 || ev.Seq < s.FirstSeq {
		s.FirstSeq = ev.Seq
	}
	if ev.Seq > s.LastSeq {
		s.LastSeq = ev.Seq
	}
	s.EventCount++
	if s.StartedAtNano == 0 {
		s.StartedAtNano = ev.TsUnixNano
	}
	if ev.TsUnixNano > s.LastEventTsNano {
		s.LastEventTsNano = ev.TsUnixNano
	}
	if ev.AppID != "" && s.AppID == "" {
		s.AppID = ev.AppID
	}
	if ev.UserID != "" && s.UserID == "" {
		s.UserID = ev.UserID
	}

	switch ev.Type {
	case EventUserMessage, EventAssistantMessage, EventSystemMessage:
		if ev.Message == nil {
			return
		}
		parts, content, toolIDs, atts := NormalizeMessageParts(ev.Message)
		s.Messages = append(s.Messages, Message{
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
			TriggerEvent: cloneAnyMap(ev.Message.TriggerEvent),
		})
		if ev.Type == EventUserMessage {
			s.TurnCount++
			if ev.Message.ClientMessageID != "" {
				if s.SeenClientMsgIDs == nil {
					s.SeenClientMsgIDs = make(map[string]uint64)
				}
				s.SeenClientMsgIDs[ev.Message.ClientMessageID] = ev.Seq
			}
			txt := content
			if txt == "" {
				for _, p := range parts {
					if p.Type == PartTypeText && p.Text != "" {
						txt = p.Text
						break
					}
				}
			}
			if len(txt) > 500 {
				txt = txt[:500] + "…"
			}
			if txt != "" {
				s.LastUserMessage = txt
			}
		}
		if ev.Type == EventSystemMessage && ev.Message.Extra != nil {
			if src, _ := ev.Message.Extra["source"].(string); src == "mode_switch" {
				if mid, ok := ev.Message.Extra["mode_id"].(string); ok {
					s.ActiveMode = mid
				}
			}
		}

	case EventToolCall:
		if ev.Tool == nil || ev.Tool.CallID == "" {
			return
		}
		s.ToolCalls[ev.Tool.CallID] = &ToolCallState{
			CallID:     ev.Tool.CallID,
			Name:       ev.Tool.Name,
			Arguments:  ev.Tool.Arguments,
			Status:     orDefault(ev.Tool.Status, "pending"),
			StartedAt:  ev.TsUnixNano,
			StartedSeq: ev.Seq,
		}

	case EventToolResult:
		if ev.Tool == nil || ev.Tool.CallID == "" {
			return
		}
		tc, ok := s.ToolCalls[ev.Tool.CallID]
		if !ok {
			tc = &ToolCallState{
				CallID:     ev.Tool.CallID,
				Name:       ev.Tool.Name,
				Status:     "completed",
				StartedSeq: ev.Seq,
				StartedAt:  ev.TsUnixNano,
			}
			s.ToolCalls[ev.Tool.CallID] = tc
		}
		tc.Status = orDefault(ev.Tool.Status, "completed")
		tc.Output = ev.Tool.Output
		tc.Error = ev.Tool.Error
		tc.CompletedAt = ev.TsUnixNano
		tc.CompletedSeq = ev.Seq
		tc.DurationMs = ev.Tool.DurationMs
		tc.UnifiedDiff = ev.Tool.UnifiedDiff
		tc.PreviousContent = ev.Tool.PreviousContent
		tc.NewContent = ev.Tool.NewContent
		if tc.DurationMs == 0 && tc.StartedAt > 0 && tc.CompletedAt > 0 {
			tc.DurationMs = (tc.CompletedAt - tc.StartedAt) / int64(time.Millisecond)
		}
		resultParts := ev.Tool.Parts
		if len(resultParts) == 0 && ev.Tool.Output != nil {
			resultParts = []MessagePart{{Type: PartTypeText, Text: formatOutput(ev.Tool.Output)}}
		}
		toolMsg := Message{
			Seq:        ev.Seq,
			StepID:     ev.StepID,
			Role:       "tool",
			TsUnixNano: ev.TsUnixNano,
			Parts: []MessagePart{{
				Type: PartTypeToolResult,
				ToolResult: &ToolResultSpec{
					ToolCallID: ev.Tool.CallID,
					Parts:      resultParts,
					Error:      ev.Tool.Error,
				},
			}},
		}
		if len(resultParts) > 0 && resultParts[0].Type == PartTypeText {
			toolMsg.Content = resultParts[0].Text
		}
		s.Messages = append(s.Messages, toolMsg)

	case EventApprovalRequest:
		if ev.Approval == nil || ev.Approval.ID == "" {
			return
		}
		s.Approvals[ev.Approval.ID] = &ApprovalState{
			ID:         ev.Approval.ID,
			Kind:       ev.Approval.Kind,
			Payload:    ev.Approval.Payload,
			Status:     orDefault(ev.Approval.Status, "pending"),
			Reason:     ev.Approval.Reason,
			CreatedAt:  ev.TsUnixNano,
			AgentID:    ev.Approval.AgentID,
			ToolName:   ev.Approval.ToolName,
			ToolParams: ev.Approval.ToolParams,
			RiskLevel:  ev.Approval.RiskLevel,
		}

	case EventApprovalGranted, EventApprovalDenied:
		if ev.Approval == nil || ev.Approval.ID == "" {
			return
		}
		ap, ok := s.Approvals[ev.Approval.ID]
		if !ok {
			ap = &ApprovalState{ID: ev.Approval.ID, Kind: ev.Approval.Kind, CreatedAt: ev.TsUnixNano}
			s.Approvals[ev.Approval.ID] = ap
		}
		if ev.Type == EventApprovalGranted {
			ap.Status = "granted"
		} else {
			ap.Status = "denied"
		}
		ap.Reason = ev.Approval.Reason
		ap.ResolvedAt = ev.TsUnixNano

	case EventMemoryRemember:
		if ev.Memory == nil || ev.Memory.Key == "" {
			return
		}
		s.Memory[ev.Memory.Key] = ev.Memory.Value

	case EventToolAllowed:
		if ev.Allowed == nil || ev.Allowed.Signature == "" {
			return
		}
		for _, sig := range s.AllowedSignatures {
			if sig == ev.Allowed.Signature {
				return
			}
		}
		s.AllowedSignatures = append(s.AllowedSignatures, ev.Allowed.Signature)

	case EventMemoryFactAdded:
		if ev.Memory == nil || ev.Memory.Fact == "" {
			return
		}
		needle := strings.ToLower(strings.TrimSpace(ev.Memory.Fact))
		for _, f := range s.Facts {
			if strings.ToLower(strings.TrimSpace(f)) == needle {
				return
			}
		}
		s.Facts = append(s.Facts, ev.Memory.Fact)
		if len(s.Facts) > maxKeyFacts {
			s.Facts = s.Facts[len(s.Facts)-maxKeyFacts:]
		}

	case EventWorkspaceWrite, EventWorkspaceEdit:
		if ev.Workspace == nil || ev.Workspace.Path == "" {
			return
		}
		s.WorkspaceFiles[ev.Workspace.Path] = &FileState{
			Path:         ev.Workspace.Path,
			ContentHash:  ev.Workspace.ContentHash,
			BaselineHash: ev.Workspace.BaselineHash,
			Bytes:        ev.Workspace.Bytes,
			UpdatedAt:    ev.TsUnixNano,
		}

	case EventWorkspaceDelete:
		if ev.Workspace == nil || ev.Workspace.Path == "" {
			return
		}
		delete(s.WorkspaceFiles, ev.Workspace.Path)

	case EventAgentSpawn:
		if ev.Agent == nil || ev.Agent.RunID == "" {
			return
		}
		s.Children = append(s.Children, ChildAgent{
			RunID:          ev.Agent.RunID,
			ParentRunID:    ev.Agent.ParentRunID,
			ParentCallID:   ev.Agent.ParentCallID,
			Kind:           ev.Agent.Kind,
			ChildSessionID: ev.Agent.ChildSessionID,
			Status:         orDefault(ev.Agent.Status, "running"),
			Depth:          ev.Agent.Depth,
			Fork:           ev.Agent.Fork,
			SpawnedAt:      ev.TsUnixNano,
			UpdatedAt:      ev.TsUnixNano,
		})

	case EventAgentProgress:
		if ev.Agent == nil || ev.Agent.RunID == "" {
			return
		}
		for i := range s.Children {
			if s.Children[i].RunID != ev.Agent.RunID {
				continue
			}
			c := &s.Children[i]
			if ev.Agent.ToolCalls > 0 {
				c.ToolCalls = ev.Agent.ToolCalls
			}
			if ev.Agent.LLMCalls > 0 {
				c.LLMCalls = ev.Agent.LLMCalls
			}
			if ev.Agent.TokensIn > 0 {
				c.TokensIn = ev.Agent.TokensIn
			}
			if ev.Agent.TokensOut > 0 {
				c.TokensOut = ev.Agent.TokensOut
			}
			if ev.Agent.Children > 0 {
				c.Children = ev.Agent.Children
			}
			if ev.Agent.DurationMs > 0 {
				c.DurationMs = ev.Agent.DurationMs
			}
			if ev.Agent.CurrentTool != "" {
				c.CurrentTool = ev.Agent.CurrentTool
			}
			if ev.Agent.Status != "" {
				c.Status = ev.Agent.Status
			}
			c.UpdatedAt = ev.TsUnixNano
			return
		}

	case EventAgentResult:
		if ev.Agent == nil || ev.Agent.RunID == "" {
			return
		}
		for i := range s.Children {
			if s.Children[i].RunID == ev.Agent.RunID {
				s.Children[i].Status = orDefault(ev.Agent.Status, "completed")
				s.Children[i].ResultSummary = ev.Agent.ResultSummary
				s.Children[i].CompletedAt = ev.TsUnixNano
				s.Children[i].UpdatedAt = ev.TsUnixNano
				s.Children[i].CurrentTool = ""
				s.Children[i].ToolCalls = ev.Agent.ToolCalls
				s.Children[i].LLMCalls = ev.Agent.LLMCalls
				s.Children[i].TokensIn = ev.Agent.TokensIn
				s.Children[i].TokensOut = ev.Agent.TokensOut
				s.Children[i].Children = ev.Agent.Children
				s.Children[i].DurationMs = ev.Agent.DurationMs
				return
			}
		}

	case EventBackgroundTask:
		if ev.Background == nil || ev.Background.TaskID == "" {
			return
		}
		bp := ev.Background
		for i := range s.BackgroundTasks {
			if s.BackgroundTasks[i].TaskID == bp.TaskID {
				s.BackgroundTasks[i].State = orDefault(bp.State, s.BackgroundTasks[i].State)
				s.BackgroundTasks[i].Error = bp.Error
				if bp.ElapsedMs > 0 {
					s.BackgroundTasks[i].ElapsedMs = bp.ElapsedMs
				}
				if bp.Label != "" {
					s.BackgroundTasks[i].Label = bp.Label
				}
				s.BackgroundTasks[i].UpdatedAtNano = ev.TsUnixNano
				return
			}
		}
		s.BackgroundTasks = append(s.BackgroundTasks, BackgroundTaskState{
			TaskID:        bp.TaskID,
			Tool:          bp.Tool,
			Label:         bp.Label,
			State:         orDefault(bp.State, "running"),
			Error:         bp.Error,
			ElapsedMs:     bp.ElapsedMs,
			StartedAtUnix: bp.StartedAtUnix,
			UpdatedAtNano: ev.TsUnixNano,
		})

	case EventWidget:
		if ev.Widget == nil || ev.Widget.ID == "" {
			return
		}
		s.Widgets[ev.Widget.ID] = &WidgetState{
			ID:    ev.Widget.ID,
			Kind:  ev.Widget.Kind,
			State: ev.Widget.State,
		}

	case EventPreview:
		if ev.Preview == nil || ev.Preview.ID == "" {
			return
		}
		s.Previews[ev.Preview.ID] = &PreviewState{
			ID:      ev.Preview.ID,
			URL:     ev.Preview.URL,
			Status:  ev.Preview.Status,
			Payload: ev.Preview.Payload,
		}

	case EventTodoAdded:
		if ev.Todo == nil || ev.Todo.ID == "" {
			return
		}
		s.Todos = append(s.Todos, Todo{
			ID:        ev.Todo.ID,
			Text:      ev.Todo.Text,
			Status:    orDefault(ev.Todo.Status, "pending"),
			CreatedAt: ev.TsUnixNano,
		})

	case EventTodoUpdated:
		if ev.Todo == nil || ev.Todo.ID == "" {
			return
		}
		for i := range s.Todos {
			if s.Todos[i].ID == ev.Todo.ID {
				if ev.Todo.Status != "" {
					s.Todos[i].Status = ev.Todo.Status
				}
				if ev.Todo.Text != "" {
					s.Todos[i].Text = ev.Todo.Text
				}
				s.Todos[i].UpdatedAt = ev.TsUnixNano
				break
			}
		}
		if s.Goal != "" && len(s.Todos) > 0 {
			allDone := true
			for _, t := range s.Todos {
				if t.Status != "done" && t.Status != "completed" {
					allDone = false
					break
				}
			}
			if allDone {
				s.Goal = ""
			}
		}

	case EventGoalSet:
		if ev.Meta != nil && ev.Meta.Workspace != "" {
			s.Workspace = ev.Meta.Workspace
		}
		if ev.Message != nil {
			s.Goal = ev.Message.Content
		}

	case EventCostUpdate, EventTokenUsage:
		if ev.Cost == nil {
			return
		}
		s.TokensIn += ev.Cost.TokensIn
		s.TokensOut += ev.Cost.TokensOut
		s.UsdTotal += ev.Cost.UsdTotal
		if ctxTok := int(ev.Cost.TokensIn + ev.Cost.TokensOut); ctxTok > 0 {
			s.ContextTokens = ctxTok
			s.ContextProviderTokens = ctxTok
		}

	case EventSessionRenamed:
		if ev.Meta != nil && ev.Meta.Title != "" {
			s.Title = ev.Meta.Title
		}

	case EventModelChanged:
			if ev.Meta != nil {
				if ev.Meta.Model == "" {
					delete(s.ModelOverrides, ev.Meta.AgentID)
					delete(s.ProviderOverrides, ev.Meta.AgentID)
					delete(s.OutputTokenOverrides, ev.Meta.AgentID)
				} else {
					if s.ModelOverrides == nil {
						s.ModelOverrides = map[string]string{}
					}
					s.ModelOverrides[ev.Meta.AgentID] = ev.Meta.Model
					if ev.Meta.Provider != "" {
						if s.ProviderOverrides == nil {
							s.ProviderOverrides = map[string]string{}
						}
						s.ProviderOverrides[ev.Meta.AgentID] = ev.Meta.Provider
					} else {
						delete(s.ProviderOverrides, ev.Meta.AgentID)
					}
					if ev.Meta.MaxOutputTokens > 0 {
						if s.OutputTokenOverrides == nil {
							s.OutputTokenOverrides = map[string]int{}
						}
						s.OutputTokenOverrides[ev.Meta.AgentID] = ev.Meta.MaxOutputTokens
					} else {
						delete(s.OutputTokenOverrides, ev.Meta.AgentID)
					}
				}
				if ev.Meta.MaxContextTokens > 0 {
					s.EntryModelWindow = ev.Meta.MaxContextTokens
				}
				if ev.Meta.ReasoningEffort != "" {
					if s.EffortOverrides == nil {
						s.EffortOverrides = map[string]string{}
					}
					s.EffortOverrides[ev.Meta.AgentID] = ev.Meta.ReasoningEffort
					if s.EntryAgent == "" || ev.Meta.AgentID == s.EntryAgent {
						s.ReasoningEffort = ev.Meta.ReasoningEffort
					}
				}
			}

	case EventSessionStarted:
		if ev.Meta != nil {
			if ev.Meta.Title != "" {
				s.Title = ev.Meta.Title
			}
			if ev.Meta.Workspace != "" {
				s.Workspace = ev.Meta.Workspace
			}
			if ev.Meta.Workdir != "" {
				s.Workdir = ev.Meta.Workdir
			}
			if ev.Meta.EntryAgent != "" {
				s.EntryAgent = ev.Meta.EntryAgent
			}
			if ev.Meta.ContextExtra != "" {
				s.ContextExtra = ev.Meta.ContextExtra
			}
		}

	case EventSessionEnded:
		s.Closed = true
		s.EndedAtNano = ev.TsUnixNano

	case EventSessionInterrupt:
		s.Interrupted = true

	case EventCompactDone:
		if ev.Compact == nil {
			return
		}
		s.Compactions = append(s.Compactions, CompactionEntry{
			Seq:            ev.Seq,
			TsUnixNano:     ev.TsUnixNano,
			CutoffSeq:      ev.Compact.CutoffSeq,
			SnapshotSHA256: ev.Compact.SnapshotSHA256,
			Binary:         ev.Compact.Binary,
			EventsBefore:   ev.Compact.EventsBefore,
			DurationMs:     ev.Compact.DurationMs,
		})

	case EventContextTokens:
		if ev.CtxTokens != nil && ev.CtxTokens.Total > 0 {
			s.ContextTokens = ev.CtxTokens.Total
			s.ContextSystemTokens = ev.CtxTokens.System
			s.ContextToolsTokens = ev.CtxTokens.Tools
			s.ContextMessageTokens = ev.CtxTokens.Messages
		}

	case EventContextSummaryPrepared:
		if ev.CtxSummary == nil || ev.CtxSummary.CoversSeq == 0 || strings.TrimSpace(ev.CtxSummary.Summary) == "" {
			return
		}
		if s.PreparedSummary == nil || ev.CtxSummary.CoversSeq > s.PreparedSummary.CoversSeq {
			s.PreparedSummary = &PreparedSummaryState{
				Summary:    ev.CtxSummary.Summary,
				CoversSeq:  ev.CtxSummary.CoversSeq,
				AtSeq:      ev.Seq,
				TsUnixNano: ev.TsUnixNano,
			}
		}

	case EventContextCompacting:
		s.CompactionInflight = true

	case EventContextCompacted:
		s.CompactionInflight = false
		if ev.CtxCompact == nil {
			return
		}
		if ev.CtxCompact.CutoffSeq == 0 {
			return
		}
		s.ContextProviderTokens = 0
		if s.ContextCompaction == nil || ev.CtxCompact.CutoffSeq >= s.ContextCompaction.CutoffSeq {
			s.ContextCompaction = &ContextCompactionState{
				CutoffSeq:  ev.CtxCompact.CutoffSeq,
				Summary:    ev.CtxCompact.Summary,
				KeepRecent: ev.CtxCompact.KeepRecent,
				Strategy:   ev.CtxCompact.Strategy,
				AtSeq:      ev.Seq,
				TsUnixNano: ev.TsUnixNano,
			}
			s.Messages = MessagesAfterCutoff(s.Messages, ev.CtxCompact.CutoffSeq)
		}
		if s.PreparedSummary != nil && ev.CtxCompact.CutoffSeq >= s.PreparedSummary.CoversSeq {
			s.PreparedSummary = nil
		}

	case EventError:
		if ev.Error == nil {
			return
		}
		s.Errors = append(s.Errors, ErrorEntry{
			Seq:        ev.Seq,
			TsUnixNano: ev.TsUnixNano,
			Code:       ev.Error.Code,
			Message:    ev.Error.Message,
			Source:     ev.Error.Source,
			Fatal:      ev.Error.Fatal,
		})

	case EventQuarantine:
		s.Partial = true

	case EventTurnStarted:
		if ev.Turn == nil || ev.Turn.TurnID == "" {
			return
		}
		s.CurrentTurnID = ev.Turn.TurnID
		s.CurrentTurnPhase = "pending"
		s.CurrentTurnStartedAtNano = ev.TsUnixNano

	case EventTurnPhaseChanged:
		if ev.Turn == nil || ev.Turn.Phase == "" {
			return
		}
		if ev.Turn.TurnID != "" && s.CurrentTurnID != "" && ev.Turn.TurnID != s.CurrentTurnID {
			return
		}
		s.CurrentTurnPhase = ev.Turn.Phase

	case EventTurnEnded:
		if ev.Turn == nil {
			return
		}
		if ev.Turn.TurnID != "" && s.CurrentTurnID != "" && ev.Turn.TurnID != s.CurrentTurnID {
			return
		}
		s.CurrentTurnID = ""
		s.CurrentTurnPhase = ""
		s.CurrentTurnStartedAtNano = 0
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func formatOutput(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
