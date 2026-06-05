package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestGoShellRouting proves the module's no-bash path end to end: with the
// built-in Go interpreter forced on, an agent's bash command flows
// params → run → goshell → result, and the commands that BROKE under the old
// PowerShell translation now work natively. No external shell is involved.
func TestGoShellRouting(t *testing.T) {
	m := New()
	m.cfg.Workdir = t.TempDir()
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	m.useGoShell = true // force the built-in interpreter regardless of host bash

	cases := []struct {
		cmd, want string
		success   bool
	}{
		{`echo hello`, "hello", true},
		{`X=5; echo $X`, "5", true},                // PS: "X=5 not recognized"
		{`export FOO=bar; echo $FOO`, "bar", true}, // PS: empty
		{`a=(x y z); echo ${a[1]}`, "y", true},     // arrays (ash can't)
		{`[[ 2 -lt 3 ]] && echo lt`, "lt", true},
		{`echo $((6*7))`, "42", true},
		{`true && echo ok`, "ok", true},
		{`false || echo recovered`, "recovered", true},
		{`echo $(echo nested)`, "nested", true},
		{`for i in 1 2 3; do printf "%s" "$i"; done`, "123", true},
		{`exit 3`, "", false},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(map[string]any{"command": c.cmd})
		res, err := m.run(context.Background(), raw)
		if err != nil {
			t.Fatalf("%q: err %v", c.cmd, err)
		}
		if res.Success != c.success {
			t.Fatalf("%q: success=%v want %v (err=%q)", c.cmd, res.Success, c.success, res.Error)
		}
		var d struct {
			Stdout string `json:"stdout"`
			Shell  string `json:"shell"`
		}
		data, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(data, &d)
		if strings.TrimSpace(d.Stdout) != c.want {
			t.Fatalf("%q: stdout=%q want %q", c.cmd, d.Stdout, c.want)
		}
	}
}
