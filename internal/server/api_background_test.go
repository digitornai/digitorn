package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// TestAPI_BackgroundTasks_DurableAcrossRestart : the resync guarantee. After a
// task runs, its lifecycle is durable — so GET /tasks reconstructs the task list
// from the on-disk event log even when the in-memory registry is gone (a fresh
// daemon process). `tasks` is the durable view ; `live` is the (now empty)
// registry.
func TestAPI_BackgroundTasks_DurableAcrossRestart(t *testing.T) {
	h := newAPIHarness(t)
	mgr := background.New()
	mgr.AttachDispatcher(bgInstantDispatcher{}) // completes immediately
	mgr.AttachSink(h.bus)
	h.daemon.background = mgr

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	tid, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app-1", UserID: "user-A", Tool: "database.sql",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitUntil(t, func() bool {
		st, _ := mgr.Status(context.Background(), sid, tid)
		return st.State == "completed"
	}, "task completed durably")

	// Simulate a daemon restart : flush events to disk, drop the in-memory
	// session state, and swap in a FRESH (empty) registry.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.flusher.Flush(ctx)
	h.bus.Drop(sid)
	freshMgr := background.New()
	freshMgr.AttachDispatcher(bgInstantDispatcher{})
	h.daemon.background = freshMgr

	code, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/tasks", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("list after restart: %d %s", code, body)
	}
	var resp struct {
		Tasks []sessionstore.BackgroundTaskState `json:"tasks"`
		Live  []meta.BackgroundStatus            `json:"live"`
		Count int                                `json:"count"`
	}
	decodeBody(t, body, &resp)
	if resp.Count != 1 || len(resp.Tasks) != 1 {
		t.Fatalf("durable tasks lost across restart : count=%d tasks=%+v", resp.Count, resp.Tasks)
	}
	if resp.Tasks[0].TaskID != tid || resp.Tasks[0].State != "completed" {
		t.Errorf("durable task wrong after restart : %+v", resp.Tasks[0])
	}
	if len(resp.Live) != 0 {
		t.Errorf("fresh registry should be empty, got %d live tasks", len(resp.Live))
	}
}

// bgBlockingDispatcher keeps a task "running" until released or cancelled,
// so the list endpoint can observe a live task.
type bgBlockingDispatcher struct{ release chan struct{} }

func (d *bgBlockingDispatcher) Dispatch(ctx context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	select {
	case <-d.release:
	case <-ctx.Done():
	}
	return runtime.ToolOutcome{Status: "completed"}
}

// TestAPI_BackgroundTasks_ListAndCancel exercises the per-session REST
// surface : the owner lists running tasks and cancels one ; a different
// user is forbidden ; an unknown task id 404s.
func TestAPI_BackgroundTasks_ListAndCancel(t *testing.T) {
	h := newAPIHarness(t)

	mgr := background.New()
	release := make(chan struct{})
	defer close(release)
	mgr.AttachDispatcher(&bgBlockingDispatcher{release: release})
	mgr.AttachSink(h.bus)
	h.daemon.background = mgr

	// Owned session.
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"t"}`)
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	// Launch a task scoped to that session (as the runtime would).
	tid, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app-1", UserID: "user-A", Tool: "database.sql",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// List as the owner : the running task shows up.
	code, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/tasks", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var listed map[string]any
	decodeBody(t, body, &listed)
	tasks, _ := listed["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d : %s", len(tasks), body)
	}
	task0 := tasks[0].(map[string]any)
	if task0["task_id"] != tid {
		t.Errorf("task_id = %v, want %v", task0["task_id"], tid)
	}
	if task0["state"] != "running" {
		t.Errorf("state = %v, want running", task0["state"])
	}

	// A different user cannot list this session's tasks.
	code, _ = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/tasks", "user-B", "")
	if code != http.StatusForbidden {
		t.Errorf("cross-user list: code = %d, want 403", code)
	}

	// Cancel as the owner.
	code, body = h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/tasks/"+tid+"/cancel", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, body)
	}
	var cancelled map[string]any
	decodeBody(t, body, &cancelled)
	if cancelled["cancelled"] != true {
		t.Errorf("cancel response: %s", body)
	}

	// Cancelling an unknown task id 404s.
	code, _ = h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/tasks/does-not-exist/cancel", "user-A", "")
	if code != http.StatusNotFound {
		t.Errorf("cancel unknown: code = %d, want 404", code)
	}
}
