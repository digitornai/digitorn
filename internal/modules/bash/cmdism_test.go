package bash

import "testing"

// TestPsNulSink : CMD `2>nul`/`>nul` must become PowerShell `$null`, never a
// file named `nul` (the reserved device that makes .NET throw the cryptic
// "FileStream was asked to open a device … com1:/lpt1:"). Real paths and quoted
// occurrences are left alone.
func TestPsNulSink(t *testing.T) {
	cases := []struct{ in, want string }{
		// CMD null device
		{`dir *.py 2>nul`, `dir *.py 2>$null`},
		{`cmd >nul`, `cmd >$null`},
		{`cmd 1>nul`, `cmd 1>$null`},
		{`cmd 2>NUL`, `cmd 2>$null`},
		{`cmd 2> nul`, `cmd 2> $null`},
		{`a 2>nul | b 2>nul`, `a 2>$null | b 2>$null`},
		{`gci 2>nul; echo done`, `gci 2>$null; echo done`},
		// bash null device (the dominant reflex)
		{`node --version 2>/dev/null`, `node --version 2>$null`},
		{`node --version >/dev/null`, `node --version >$null`},
		{`node --version >/dev/null 2>&1`, `node --version >$null 2>&1`},
		{`grep x f 2>/dev/null | wc -l`, `grep x f 2>$null | wc -l`},
		{`cmd 2> /dev/null`, `cmd 2> $null`},
		// untouched:
		{`echo "2>nul"`, `echo "2>nul"`},                   // quoted literal
		{`echo x >nul.txt`, `echo x >nul.txt`},             // real file, not the device
		{`echo x >/dev/null.bak`, `echo x >/dev/null.bak`}, // real path, not the device
		{`cmd 2>&1`, `cmd 2>&1`},                           // stderr→stdout merge
		{`echo nul`, `echo nul`},                           // bare word, no redirect
	}
	for _, c := range cases {
		if got := psNulSink(c.in); got != c.want {
			t.Errorf("psNulSink(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// TestPsEnv : bash env-var idioms translate to PowerShell $env: assignment.
func TestPsEnv(t *testing.T) {
	cases := []struct{ in, want string }{
		{`export FOO=bar`, `$env:FOO='bar'`},
		{`export NODE_ENV=production`, `$env:NODE_ENV='production'`},
		{`export FOO="a b"`, `$env:FOO="a b"`},                         // already quoted → kept
		{`export FOO='x'`, `$env:FOO='x'`},                             // already quoted
		{`export FOO=bar && node x.js`, `$env:FOO='bar' && node x.js`}, // psChain handles && after
		{`FOO=bar node -e "1"`, `$env:FOO='bar'; node -e "1"`},         // inline env
		{`echo hi; export A=b`, `echo hi; $env:A='b'`},
		// left untouched (the bashism guard handles these):
		{`export PATH=$PATH:/x`, `export PATH=$PATH:/x`}, // $-value → not rewritten
		{`node --version`, `node --version`},
		{`echo FOO=bar`, `echo FOO=bar`}, // echo's arg, not an assignment leader
	}
	for _, c := range cases {
		if got := psEnv(c.in); got != c.want {
			t.Errorf("psEnv(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// TestBashismHint : un-translatable bash syntax gets a clear hint ; valid
// PowerShell (and the things we DO translate) do not.
func TestBashismHint(t *testing.T) {
	flagged := []string{
		`for f in a b c; do echo $f; done`,
		`if [ -d . ]; then echo yes; fi`,
		`[ -f go.mod ] && echo has`,
		`test -f go.mod`,
		`source venv/bin/activate`,
		`export PATH=$PATH:/x`, // survived translation (has $)
		`while [ -e lock ]; do sleep 1; done`,
	}
	for _, c := range flagged {
		if bashismHint(c) == "" {
			t.Errorf("expected a bashism hint for: %q", c)
		}
	}
	clean := []string{
		`node --version`,
		`if ($x) { Write-Output ok }`,       // PowerShell if
		`foreach ($f in $list) { echo $f }`, // PowerShell loop
		`$env:FOO='bar'; node x.js`,         // already-translated env
		`Get-ChildItem | Where-Object { $_ }`,
		`git status && echo done`,
		`echo "test -f is just text"`,                // inside a quoted arg
		`bash -c "for i in 1 2 3; do echo $i; done"`, // explicit bash escape hatch
		`bash -c 'if [ -d . ]; then echo yes; fi'`,   // single-quoted bash -c
		`sh -c "test -f x && echo y"`,                // sh -c
		`cmd /c "for %i in (1 2 3) do echo %i"`,      // cmd /c
	}
	for _, c := range clean {
		if msg := bashismHint(c); msg != "" {
			t.Errorf("must NOT flag %q — got %q", c, msg)
		}
	}
}
