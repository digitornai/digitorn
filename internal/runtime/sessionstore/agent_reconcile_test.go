package sessionstore

import (
	"os"
	"testing"
	"time"
)

func TestColdLoad_ReconcilesOrphanRunningAgents(t *testing.T) {
	dir := t.TempDir()
	paths := NewPaths(dir)
	sid := "crash-session"

	if err := os.MkdirAll(paths.SessionDir(sid), 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	events := []Event{
		{Seq: 1, TsUnixNano: now, Type: EventAgentSpawn, SessionID: sid,
			Agent: &AgentPayload{RunID: "researcher#aaa", Kind: "researcher", Status: "running"}},
		{Seq: 2, TsUnixNano: now + 1, Type: EventAgentSpawn, SessionID: sid,
			Agent: &AgentPayload{RunID: "writer#bbb", Kind: "writer", Status: "running", ParentRunID: "researcher#aaa", Depth: 1}},
		{Seq: 3, TsUnixNano: now + 2, Type: EventAgentResult, SessionID: sid,
			Agent: &AgentPayload{RunID: "writer#bbb", Status: "completed", ResultSummary: "done", TokensIn: 42}},
	}
	if _, err := w.Write(events); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	res, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	children := res.State.Snapshot().Children
	byRun := map[string]ChildAgent{}
	for _, c := range children {
		byRun[c.RunID] = c
	}
	if got := byRun["researcher#aaa"].Status; got != "interrupted" {
		t.Errorf("orphan running agent must be reconciled to interrupted, got %q", got)
	}
	if got := byRun["writer#bbb"].Status; got != "completed" {
		t.Errorf("completed agent must keep its terminal status, got %q", got)
	}
	if byRun["writer#bbb"].TokensIn != 42 {
		t.Errorf("completed agent telemetry lost: %+v", byRun["writer#bbb"])
	}
}

func TestColdLoad_ReconcilesOrphanBackgroundTasks(t *testing.T) {
	dir := t.TempDir()
	paths := NewPaths(dir)
	sid := "bg-crash-session"

	if err := os.MkdirAll(paths.SessionDir(sid), 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	events := []Event{
		{Seq: 1, TsUnixNano: now, Type: EventBackgroundTask, SessionID: sid,
			Background: &BackgroundTaskPayload{TaskID: "t1", Tool: "database.sql", State: "running"}},
		{Seq: 2, TsUnixNano: now + 1, Type: EventBackgroundTask, SessionID: sid,
			Background: &BackgroundTaskPayload{TaskID: "t2", Tool: "http.get", State: "running"}},
		{Seq: 3, TsUnixNano: now + 2, Type: EventBackgroundTask, SessionID: sid,
			Background: &BackgroundTaskPayload{TaskID: "t2", State: "completed", ElapsedMs: 120}},
	}
	if _, err := w.Write(events); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	res, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tasks := res.State.Snapshot().BackgroundTasks
	byID := map[string]BackgroundTaskState{}
	for _, tk := range tasks {
		byID[tk.TaskID] = tk
	}
	if len(byID) != 2 {
		t.Fatalf("expected 2 durable tasks, got %d : %+v", len(byID), tasks)
	}
	if got := byID["t1"].State; got != "interrupted" {
		t.Errorf("orphan running task must be reconciled to interrupted, got %q", got)
	}
	if got := byID["t2"].State; got != "completed" {
		t.Errorf("completed task must keep its terminal state, got %q", got)
	}
	if byID["t2"].ElapsedMs != 120 {
		t.Errorf("completed task telemetry lost: %+v", byID["t2"])
	}
}
