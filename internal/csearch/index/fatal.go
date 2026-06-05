package index

import "fmt"

// fatal / fatalf replace upstream codesearch's log.Fatal / log.Fatalf — which
// called os.Exit and would take down the ENTIRE daemon on a corrupt, oversized,
// or unwritable index. Panicking instead lets the caller's boundary recover()
// and degrade gracefully (fall back to a live scan, rebuild the index). This is
// the only behavioural change to the vendored library ; everything else is the
// upstream BSD source verbatim (see LICENSE).
func fatal(v ...any)                 { panic(fmt.Sprint(v...)) }
func fatalf(format string, v ...any) { panic(fmt.Sprintf(format, v...)) }
