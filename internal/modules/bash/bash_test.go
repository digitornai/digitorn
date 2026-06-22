package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

func testShell(t *testing.T, maxOut int) *shell {
	t.Helper()
	kind, path, err := detectShell("bash") // these assertions are bash-specific
	if err != nil || kind != "bash" {
		t.Skip("no bash available")
	}
	sh, err := newShell(kind, path, t.TempDir(), buildEnv(nil), maxOut)
	if err != nil {
		t.Fatalf("newShell: %v", err)
	}
	t.Cleanup(sh.close)
	return sh
}

func run(t *testing.T, sh *shell, cmd string, timeout time.Duration) cmdResult {
	t.Helper()
	res, err := sh.run(context.Background(), cmd, timeout)
	if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	return res
}

func TestShell_BasicOutputAndExit(t *testing.T) {
	sh := testShell(t, 1<<20)
	res := run(t, sh, "echo hello", 10*time.Second)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("got exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}
	res = run(t, sh, "echo oops 1>&2; false", 10*time.Second)
	if res.ExitCode != 1 {
		t.Fatalf("want exit 1, got %d", res.ExitCode)
	}
	if strings.TrimSpace(res.Stderr) != "oops" {
		t.Fatalf("stderr=%q", res.Stderr)
	}
}

func TestShell_PersistsCwdVarsAndFuncs(t *testing.T) {
	sh := testShell(t, 1<<20)
	sub := "sub_" + fmt.Sprint(time.Now().UnixNano())
	run(t, sh, "mkdir "+sub+" && cd "+sub, 10*time.Second)
	res := run(t, sh, "basename \"$PWD\"", 10*time.Second)
	if strings.TrimSpace(res.Stdout) != sub {
		t.Fatalf("cwd did not persist: pwd basename=%q want %q (cwd field=%q)", res.Stdout, sub, res.Cwd)
	}
	run(t, sh, "export GREETING=bonjour", 10*time.Second)
	run(t, sh, "myfunc() { echo from-func; }", 10*time.Second)
	res = run(t, sh, "echo \"$GREETING\"; myfunc", 10*time.Second)
	if !strings.Contains(res.Stdout, "bonjour") || !strings.Contains(res.Stdout, "from-func") {
		t.Fatalf("export/func did not persist: %q", res.Stdout)
	}
}

func TestShell_StdinIsolation_NoHangOnCat(t *testing.T) {
	sh := testShell(t, 1<<20)
	done := make(chan cmdResult, 1)
	go func() {
		r, _ := sh.run(context.Background(), "cat; echo after-cat", 10*time.Second)
		done <- r
	}()
	select {
	case r := <-done:
		if r.TimedOut {
			t.Fatalf("cat hung (timed out) — stdin not isolated")
		}
		if !strings.Contains(r.Stdout, "after-cat") {
			t.Fatalf("stdout=%q", r.Stdout)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cat blocked the shell — framing bytes were consumed by the command")
	}
}

func TestShell_TimeoutKillsTreePromptly(t *testing.T) {
	sh := testShell(t, 1<<20)
	start := time.Now()
	res, err := sh.run(context.Background(), "sleep 30", 1*time.Second)
	if !errors.Is(err, errTimeout) || !res.TimedOut {
		t.Fatalf("expected timeout, got err=%v res=%+v", err, res)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("timeout took too long: %v (process tree not killed promptly)", elapsed)
	}
	if !sh.isClosed() {
		t.Fatal("shell not closed after a timeout kill")
	}
}

func TestModule_TimeoutThenRecover(t *testing.T) {
	m := testModule(t)
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "s"})
	start := time.Now()
	raw, _ := json.Marshal(runParams{Command: "sleep 30", TimeoutSeconds: 1})
	res, _ := m.run(ctx, raw)
	rr, ok := res.Data.(runResult)
	if !ok || !rr.TimedOut {
		t.Fatalf("expected a timed-out result, got %T %+v (err=%q)", res.Data, res.Data, res.Error)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("timeout kill too slow: %v", elapsed)
	}
	// the module must transparently recreate the shell and keep working
	rr2 := invoke(t, m, "s", "echo recovered")
	if strings.TrimSpace(rr2.Stdout) != "recovered" {
		t.Fatalf("module did not recover after timeout: %q", rr2.Stdout)
	}
}

func TestShell_ContextCancel_KillsTreeAndReportsCancelled(t *testing.T) {
	sh := testShell(t, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	type out struct {
		res cmdResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, e := sh.run(ctx, "sleep 30", 60*time.Second)
		ch <- out{r, e}
	}()
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	cancel() // the user / caller cancels the running command
	select {
	case o := <-ch:
		if !errors.Is(o.err, errCancelled) || !o.res.Cancelled {
			t.Fatalf("want cancelled, got err=%v res=%+v", o.err, o.res)
		}
		if o.res.TimedOut {
			t.Fatal("cancel was misreported as a timeout")
		}
		if elapsed := time.Since(start); elapsed > 6*time.Second {
			t.Fatalf("cancel did not kill the tree promptly: %v", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not unblock the command (tree not killed)")
	}
	if !sh.isClosed() {
		t.Fatal("shell not closed after cancel")
	}
}

func TestShell_StderrHeavy_NoDeadlock(t *testing.T) {
	sh := testShell(t, 8<<20)
	done := make(chan cmdResult, 1)
	go func() {
		r, _ := sh.run(context.Background(), "seq 1 100000 1>&2; echo done", 30*time.Second)
		done <- r
	}()
	select {
	case r := <-done:
		if r.TimedOut {
			t.Fatal("stderr-heavy command deadlocked")
		}
		if strings.TrimSpace(r.Stdout) != "done" {
			t.Fatalf("stdout=%q", r.Stdout)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("deadlock: stderr pipe filled while only stdout was drained")
	}
}

func TestShell_BoundedOutput_Truncates(t *testing.T) {
	sh := testShell(t, 256)
	res := run(t, sh, "seq 1 10000", 15*time.Second)
	if !strings.Contains(res.Stdout, "[truncated") {
		t.Fatalf("expected truncation marker, stdout len=%d", len(res.Stdout))
	}
	if len(res.Stdout) > 256+64 {
		t.Fatalf("output not bounded: %d bytes", len(res.Stdout))
	}
}

func TestShell_ExitCommandDegrades(t *testing.T) {
	sh := testShell(t, 1<<20)
	_, err := sh.run(context.Background(), "exit 7", 10*time.Second)
	if !errors.Is(err, errShellExited) {
		t.Fatalf("want errShellExited, got %v", err)
	}
}

func TestEnv_AllowlistBlocksSecretsKeepsEssentials(t *testing.T) {
	t.Setenv("MY_PRIVATE_TOKEN", "super-secret-value")
	sh := testShell(t, 1<<20)
	res := run(t, sh, "echo \"[$MY_PRIVATE_TOKEN]\"", 10*time.Second)
	if strings.Contains(res.Stdout, "super-secret-value") {
		t.Fatalf("secret leaked into the shell env: %q", res.Stdout)
	}
	res = run(t, sh, "echo \"$CI\"", 10*time.Second)
	if strings.TrimSpace(res.Stdout) != "true" {
		t.Fatalf("non-interactive default missing: CI=%q", res.Stdout)
	}
	res = run(t, sh, "test -n \"$PATH\" && echo has-path", 10*time.Second)
	if strings.TrimSpace(res.Stdout) != "has-path" {
		t.Fatal("PATH was not passed through")
	}
}

func testModule(t *testing.T) *Module {
	t.Helper()
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workdir": t.TempDir(), "shell": "bash"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if m.path == "" || m.kind != "bash" {
		t.Skip("no bash available")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

func invoke(t *testing.T, m *Module, sessionID, command string) runResult {
	t.Helper()
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: sessionID})
	raw, _ := json.Marshal(runParams{Command: command})
	res, err := m.run(ctx, raw)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	rr, ok := res.Data.(runResult)
	if !ok {
		t.Fatalf("unexpected data type %T (err=%q)", res.Data, res.Error)
	}
	return rr
}

// TestModule_OneShotNoStateLeak : one-shot execution means state set in one
// call is visible in NO later call — neither another session (isolation) nor the
// same session (no persistence). Each command is its own fresh process, so a
// stray `export`/`cd` can never bleed forward.
func TestModule_OneShotNoStateLeak(t *testing.T) {
	m := testModule(t)
	invoke(t, m, "sessionA", "export SECRET=alpha")
	rb := invoke(t, m, "sessionB", "echo \"[$SECRET]\"")
	if strings.Contains(rb.Stdout, "alpha") {
		t.Fatalf("session B saw another call's state: %q", rb.Stdout)
	}
	ra := invoke(t, m, "sessionA", "echo \"[$SECRET]\"")
	if strings.Contains(ra.Stdout, "alpha") {
		t.Fatalf("state leaked across one-shot calls (must NOT persist): %q", ra.Stdout)
	}
}

func TestModule_BackgroundIgnoresForegroundTimeout(t *testing.T) {
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workdir": t.TempDir(), "timeout_seconds": 1}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !m.HasShell() {
		t.Skip("no POSIX shell available")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	// Foreground default timeout is 1s; a BACKGROUND command must NOT inherit it,
	// or a dev server would be killed mid-run (the real bug Paul hit).
	ctx := tool.WithBackground(tool.WithIdentity(context.Background(), tool.Identity{AppID: "a", SessionID: "s"}))
	raw, _ := json.Marshal(runParams{Command: "sleep 3; echo done"})
	start := time.Now()
	res, err := m.run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	rr := res.Data.(runResult)
	if rr.TimedOut {
		t.Fatalf("background task was killed by the foreground 1s timeout: %+v", rr)
	}
	if !strings.Contains(rr.Stdout, "done") {
		t.Fatalf("background command did not complete: %q", rr.Stdout)
	}
	if time.Since(start) < 2500*time.Millisecond {
		t.Fatalf("returned in %v — it was cut short instead of running ~3s", time.Since(start))
	}
}

// TestModule_OneShotResetsCwdAndEnv pins the one-shot contract: NEITHER the
// working directory NOR exported vars carry across calls — every command starts
// fresh at the workspace root. So a stale `cd` can't redirect a later command's
// relative paths, and a stray `export` can't bleed forward. The agent sets up
// state inline in one command (`cd proj && export X=1 && cmd`).
func TestModule_OneShotResetsCwdAndEnv(t *testing.T) {
	m := testModule(t)
	const sess = "anchor"
	invoke(t, m, sess, "export MARK=anchored && mkdir -p cwdsub && cd cwdsub")
	rr := invoke(t, m, sess, `pwd; echo "mark=[$MARK]"`)
	if strings.Contains(rr.Stdout, "mark=[anchored]") {
		t.Fatalf("exported var leaked across one-shot calls (must reset): %q", rr.Stdout)
	}
	if strings.Contains(rr.Stdout, "cwdsub") {
		t.Fatalf("cwd leaked across calls — it must reset to the workspace root: %q", rr.Stdout)
	}
}

// TestModule_BackgroundStartsAtRoot : a backgrounded command ignores any prior
// foreground cd and starts at the workspace root, like every other call.
func TestModule_BackgroundStartsAtRoot(t *testing.T) {
	m := testModule(t)
	const sess = "bgroot"
	invoke(t, m, sess, "mkdir -p deep/nested && cd deep/nested")
	ctx := tool.WithBackground(tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: sess}))
	raw, _ := json.Marshal(runParams{Command: "pwd"})
	res, err := m.run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	rr := res.Data.(runResult)
	if strings.Contains(rr.Stdout, "nested") {
		t.Fatalf("background command inherited a stale cwd instead of starting at root: %q", rr.Stdout)
	}
}

func TestPSChain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"npm install", "npm install"},
		{"a | b", "a | b"},       // pipe untouched
		{"echo x &", "echo x &"}, // single & untouched
		{"cd app && npm install", "cd app; if ($?) { npm install }"},
		{"a && b && c", "a; if ($?) { b; if ($?) { c } }"},
		{"a || b", "a; if (-not $?) { b }"},
		{`git commit -m "fix && run"`, `git commit -m "fix && run"`}, // quoted && untouched
	}
	for _, c := range cases {
		if got := psChain(c.in); got != c.want {
			t.Errorf("psChain(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

func TestModule_ConcurrentSessions_Race(t *testing.T) {
	m := testModule(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sess := fmt.Sprintf("s%d", n)
			ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: sess})
			for j := 0; j < 10; j++ {
				raw, _ := json.Marshal(runParams{Command: fmt.Sprintf("echo %d-%d", n, j)})
				if _, err := m.run(ctx, raw); err != nil {
					t.Errorf("run: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestGuard_RefusesDestructivePatterns(t *testing.T) {
	for _, bad := range []string{"rm -rf /", "rm -rf /*", "RM -RF /", ":(){ :|:& };:", "mkfs.ext4 /dev/sda", "dd if=/dev/zero of=/dev/sda"} {
		if checkCommand(bad) == nil {
			t.Errorf("guard missed destructive command: %q", bad)
		}
	}
	for _, good := range []string{"rm -rf ./build", "git status", "go test ./...", "rm file.txt"} {
		if err := checkCommand(good); err != nil {
			t.Errorf("guard refused a safe command %q: %v", good, err)
		}
	}
}

func TestDetached_BasicOutput(t *testing.T) {
	kind, path, err := detectShell("bash") // these assertions are bash-specific
	if err != nil || kind != "bash" {
		t.Skip("no bash available")
	}
	res, err := runDetached(context.Background(), kind, path, "echo detached-ok", t.TempDir(), buildEnv(nil), 1<<20, "", 10*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "detached-ok" {
		t.Fatalf("exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}
}

func TestDetached_InputFedToStdin(t *testing.T) {
	kind, path, err := detectShell("bash") // these assertions are bash-specific
	if err != nil || kind != "bash" {
		t.Skip("no bash available")
	}
	res, err := runDetached(context.Background(), kind, path, "read x; echo \"GOT:$x\"", t.TempDir(), buildEnv(nil), 1<<20, "hello-stdin\n", 10*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "GOT:hello-stdin") {
		t.Fatalf("input was not delivered to stdin: %q", res.Stdout)
	}
}

func TestDetached_CancelKillsTree(t *testing.T) {
	kind, path, err := detectShell("bash") // these assertions are bash-specific
	if err != nil || kind != "bash" {
		t.Skip("no bash available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	type o struct {
		r cmdResult
		e error
	}
	ch := make(chan o, 1)
	go func() {
		r, e := runDetached(ctx, kind, path, "sleep 30", t.TempDir(), buildEnv(nil), 1<<20, "", 60*time.Second, 0)
		ch <- o{r, e}
	}()
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case got := <-ch:
		if !errors.Is(got.e, errCancelled) || !got.r.Cancelled {
			t.Fatalf("want cancelled, got e=%v r=%+v", got.e, got.r)
		}
		if time.Since(start) > 6*time.Second {
			t.Fatalf("detached cancel too slow: %v", time.Since(start))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("detached cancel did not kill the process")
	}
}

func TestModule_BackgroundRoutesToDetached(t *testing.T) {
	m := testModule(t)
	ctx := tool.WithBackground(tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "bg"}))
	raw, _ := json.Marshal(runParams{Command: "echo bg-ok"})
	res, err := m.run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	rr, ok := res.Data.(runResult)
	if !ok || strings.TrimSpace(rr.Stdout) != "bg-ok" {
		t.Fatalf("unexpected result %T %+v", res.Data, res.Data)
	}
	m.mu.Lock()
	n := len(m.shells)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("background dispatch created %d session shells (want 0 — it must use an independent process)", n)
	}
}

func TestRunParams_FlexibleTimeout(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`{"command":"x","timeout_seconds":"60"}`, 60},
		{`{"command":"x","timeout_seconds":60}`, 60},
		{`{"command":"x","timeout_seconds":"60.0"}`, 60},
		{`{"command":"x","timeout_seconds":""}`, 0},
		{`{"command":"x","timeout_seconds":null}`, 0},
		{`{"command":"x"}`, 0},
		{`{"command":"x","timeout_seconds":"abc"}`, 0},
		{`{"command":"x","timeout_seconds":" 90 "}`, 90},
	}
	for _, c := range cases {
		var p runParams
		if err := json.Unmarshal([]byte(c.raw), &p); err != nil {
			t.Fatalf("unmarshal %s: %v", c.raw, err)
		}
		if int(p.TimeoutSeconds) != c.want {
			t.Fatalf("%s -> %d, want %d", c.raw, int(p.TimeoutSeconds), c.want)
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
