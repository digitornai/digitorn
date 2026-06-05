package goshell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sh runs a script in dir with the full environment and returns code+stdout+stderr.
func sh(t *testing.T, dir, script string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code, err := Run(context.Background(), script, dir, os.Environ(), nil, &out, &errb)
	if err != nil {
		t.Logf("setup err for %q: %v", script, err)
	}
	return code, out.String(), errb.String()
}

func trim(s string) string { return strings.TrimRight(s, "\r\n") }

// ---- 1. Shell language: the things an agent leans on -------------------------

func TestAdv_Language(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, script, want string }{
		{"single_quote", `echo 'a $b c'`, "a $b c"},
		{"double_quote_expand", `b=X; echo "a ${b} c"`, "a X c"},
		{"nested_quotes", `echo "outer 'inner' end"`, "outer 'inner' end"},
		{"ansi_c_quote", `printf '%s' $'a\tb'`, "a\tb"},
		{"default_param", `echo ${missing:-fallback}`, "fallback"},
		{"alt_param", `set=1; echo ${set:+yes}`, "yes"},
		{"len_param", `s=hello; echo ${#s}`, "5"},
		{"substr", `s=hello; echo ${s:1:3}`, "ell"},
		{"replace", `s=a-b-c; echo ${s//-/_}`, "a_b_c"},
		{"upper", `s=abc; echo ${s^^}`, "ABC"},
		{"arith_compare", `if (( 3 > 2 )); then echo gt; fi`, "gt"},
		{"arith_assign", `x=0; (( x += 7 )); echo $x`, "7"},
		{"c_style_for", `for ((i=0;i<3;i++)); do printf "%d" $i; done`, "012"},
		{"case_stmt", `x=b; case $x in a) echo A;; b) echo B;; esac`, "B"},
		{"func_return", `f(){ return 42; }; f; echo $?`, "42"},
		{"func_args", `g(){ echo "$1-$2"; }; g hi yo`, "hi-yo"},
		{"local_var", `h(){ local v=inside; echo $v; }; h`, "inside"},
		{"while_read", `printf 'l1\nl2\n' | while read x; do echo "[$x]"; done`, "[l1]\n[l2]"},
		{"array_iterate", `a=(p q r); for e in "${a[@]}"; do printf "%s" "$e"; done`, "pqr"},
		{"array_len", `a=(p q r s); echo ${#a[@]}`, "4"},
		{"assoc_array", `declare -A m; m[k]=v; echo ${m[k]}`, "v"},
		{"cmd_subst_nested", `echo $(echo $(echo deep))`, "deep"},
		{"brace_expand", `echo {1..4}`, "1 2 3 4"},
		{"brace_list", `echo pre-{a,b,c}-post`, "pre-a-post pre-b-post pre-c-post"},
		{"here_string", `cat <<< "from-herestring"`, "from-herestring"},
		{"heredoc", "cat <<EOF\nh1\nh2\nEOF", "h1\nh2"},
		{"pipefail_off", `false | true; echo $?`, "0"},
		{"set_pipefail", `set -o pipefail; false | true; echo $?`, "1"},
		{"logical_group", `{ echo a; echo b; }`, "a\nb"},
		{"subshell_group", `(echo x; echo y)`, "x\ny"},
		{"param_indirect", `v=value; ref=v; echo ${!ref}`, "value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errb := sh(t, dir, c.script)
			if code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb)
			}
			if trim(out) != c.want {
				t.Fatalf("out=%q want %q", out, c.want)
			}
		})
	}
}

// ---- 2. Redirections & files (all inside temp) ------------------------------

func TestAdv_Redirections(t *testing.T) {
	dir := t.TempDir()
	scripts := []struct{ name, script, want string }{
		{"write_read", `echo hi > f.txt; while read l; do echo "got:$l"; done < f.txt`, "got:hi"},
		{"append", `echo a > g.txt; echo b >> g.txt; while read l; do printf "%s" "$l"; done < g.txt`, "ab"},
		{"stderr_to_stdout", `{ echo out; echo err >&2; } 2>&1`, "out\nerr"},
		{"discard_stderr", `{ echo keep; echo drop >&2; } 2>/dev/null`, "keep"},
		{"here_to_file", "cat > h.txt <<EOF\nzz\nEOF\nwhile read l; do echo $l; done < h.txt", "zz"},
	}
	for _, c := range scripts {
		t.Run(c.name, func(t *testing.T) {
			code, out, errb := sh(t, dir, c.script)
			if code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb)
			}
			if trim(out) != c.want {
				t.Fatalf("out=%q want %q", out, c.want)
			}
		})
	}
}

// ---- 3. Globbing (real files in temp) ---------------------------------------

func TestAdv_Globbing(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.md"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errb := sh(t, dir, `for f in *.txt; do printf "%s\n" "$f"; done`)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	got := strings.Fields(out)
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "b.txt" {
		t.Fatalf("glob out=%q want [a.txt b.txt]", out)
	}
}

// ---- 4. Working directory & cd (read-only checks, no escapes) ---------------

func TestAdv_Workdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// cd into a subdir within one command, create a file there, verify location.
	code, out, errb := sh(t, dir, `cd sub && echo here > made.txt && echo "pwd-has-sub:$([ -f made.txt ] && echo yes)"`)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "yes") {
		t.Fatalf("cd+create failed: out=%q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "made.txt")); err != nil {
		t.Fatalf("file not created in subdir: %v", err)
	}
}

// ---- 5. Timeout / cancellation (BOUNDED — cannot hang the machine) ----------

func TestAdv_TimeoutHonored(t *testing.T) {
	dir := t.TempDir()
	// A finite-but-long CPU loop. If goshell honors ctx, it is cut short well
	// before completing. The loop is finite so even if ctx were ignored it
	// terminates (no infinite hang).
	script := `i=0; while [ $i -lt 30000000 ]; do i=$((i+1)); done; echo finished`

	type res struct {
		code int
		dur  time.Duration
	}
	ch := make(chan res, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		start := time.Now()
		var out, errb bytes.Buffer
		code, _ := Run(ctx, script, dir, os.Environ(), nil, &out, &errb)
		ch <- res{code, time.Since(start)}
	}()

	select {
	case r := <-ch:
		t.Logf("timeout test: returned in %v with exit=%d", r.dur, r.code)
		if r.dur > 5*time.Second {
			t.Fatalf("goshell did NOT honor the 400ms timeout (ran %v) — loop-blocking risk", r.dur)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("goshell hung past 20s safety deadline — context not honored")
	}
}

// ---- 6. Concurrency: many shells at once (the 1M-agents concern), -race -----

func TestAdv_Concurrent(t *testing.T) {
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			dir := t.TempDir()
			script := fmt.Sprintf(`x=%d; y=$((x*2)); echo $y > r.txt; while read v; do printf "%%s" "$v"; done < r.txt`, i)
			var out, errb bytes.Buffer
			code, err := Run(context.Background(), script, dir, os.Environ(), nil, &out, &errb)
			if err != nil || code != 0 {
				errs[i] = fmt.Sprintf("g%d: code=%d err=%v stderr=%q", i, code, err, errb.String())
				return
			}
			if trim(out.String()) != fmt.Sprintf("%d", i*2) {
				errs[i] = fmt.Sprintf("g%d: out=%q want %d", i, out.String(), i*2)
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != "" {
			t.Fatal(e)
		}
	}
}

// ---- 7. Output capping (runaway output can't blow memory upstream) ----------

func TestAdv_LargeOutput(t *testing.T) {
	dir := t.TempDir()
	// Print ~1MB without any external tool (pure shell loop doubling a string).
	code, out, _ := sh(t, dir, `s=ABCDEFGH; for i in $(seq 1 17); do s="$s$s"; done; printf "%s" "$s" | wc -c 2>/dev/null || printf "%s" "${#s}"`)
	t.Logf("large-output exit=%d, length-ish=%q", code, trim(out))
}

// ---- 8. Unicode round-trip --------------------------------------------------

func TestAdv_Unicode(t *testing.T) {
	dir := t.TempDir()
	const payload = "héllo-世界-🚀-café"
	code, out, errb := sh(t, dir, `printf '%s' "`+payload+`"`)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if trim(out) != payload {
		t.Fatalf("unicode mangled: got %q want %q", out, payload)
	}
}

// ---- 9. LIMITS MAP: external tools & coreutils (logged, not asserted) -------

func TestAdv_LimitsMap(t *testing.T) {
	dir := t.TempDir()
	full := os.Environ()
	noGit := stripGitFromPath(full) // simulate a host without Git Bash

	run := func(env []string, script string) string {
		var out, errb bytes.Buffer
		code, _ := Run(context.Background(), script, dir, env, nil, &out, &errb)
		if code == 0 {
			return "OK"
		}
		return fmt.Sprintf("FAIL(%d:%s)", code, trim(firstLine(errb.String())))
	}
	probe := func(label, script string) {
		t.Logf("%-16s  WITH-git=%-10s  NO-git=%s", label, run(full, script), run(noGit, script))
	}

	t.Log("=== external dev tools (resolved from PATH) ===")
	probe("git", `git --version`)
	probe("node", `node --version`)
	probe("npm", `npm --version`)
	probe("python", `python --version`)
	probe("go", `go version`)

	t.Log("=== GNU coreutils (the gap without Git Bash / busybox) ===")
	probe("tr", `echo x | tr a-z A-Z`)
	probe("sed", `echo abc | sed 's/b/B/'`)
	probe("awk", `echo abc | awk '{print $0}'`)
	probe("sort", `printf 'b\na\n' | sort`)
	probe("wc", `echo abc | wc -c`)
	probe("cut", `echo a,b | cut -d, -f2`)
	probe("grep", `echo abc | grep b`)
	probe("date", `date +%s`)
	probe("which", `which echo`)
	probe("xargs", `echo a | xargs echo`)
	probe("sleep", `sleep 0.01`)
	probe("curl", `curl --version`)

	t.Log("=== advanced shell features ===")
	probe("proc_subst", `cat <(echo from-procsubst)`)
	probe("background", `(sleep 0.01 &); echo after-bg`)
}

func stripGitFromPath(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], "PATH") {
			parts := strings.Split(kv[i+1:], string(os.PathListSeparator))
			kept := parts[:0]
			for _, p := range parts {
				if !strings.Contains(strings.ToLower(p), "git") {
					kept = append(kept, p)
				}
			}
			out = append(out, "PATH="+strings.Join(kept, string(os.PathListSeparator)))
			continue
		}
		out = append(out, kv)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// sanity: prove the external-exec path is genuinely exec'ing a real binary.
func TestAdv_ExternalIsReal(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	code, out, _ := sh(t, t.TempDir(), `go version`)
	if code != 0 || !strings.Contains(out, "go version") {
		t.Fatalf("external go exec failed: code=%d out=%q", code, out)
	}
}
