package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/dbaccess"
)

func main() {
	url := "postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable"
	ctx := context.Background()
	mgr := dbaccess.NewManager(2, 30*time.Minute) // bound = 2 across ALL apps
	defer mgr.Shutdown()

	cfg := dbaccess.ConnConfig{Name: "prod", Kind: "postgres", DSN: url,
		Security: dbaccess.SecurityPolicy{Mode: "read_only"}}

	dbA, err := mgr.Named(ctx, "appA", cfg)
	if err != nil {
		fmt.Println("appA open FAILED:", err)
		os.Exit(2)
	}
	if _, err := dbA.Query(ctx, "SELECT 1"); err != nil {
		fmt.Println("appA baseline query FAILED:", err)
		os.Exit(2)
	}
	fmt.Println("step1: appA opened + baseline query OK")
	time.Sleep(3 * time.Millisecond)

	if _, err := mgr.Named(ctx, "appB", cfg); err != nil {
		fmt.Println("appB open FAILED:", err)
		os.Exit(2)
	}
	fmt.Println("step2: appB opened (pool now {appA,appB}=2 == max)")
	time.Sleep(3 * time.Millisecond)

	if _, err := mgr.Named(ctx, "appC", cfg); err != nil {
		fmt.Println("appC open FAILED:", err)
		os.Exit(2)
	}
	fmt.Println("step3: appC opened (pool would be 3 > max(2) -> evictLocked runs)")

	_, stillPooled := mgr.Get("appA", "prod")
	fmt.Printf("step4: appA still in pool after appC opened? %v\n", stillPooled)

	_, qerr := dbA.Query(ctx, "SELECT 1")
	fmt.Printf("step5: appA query AFTER appC opened -> err=%v\n", qerr)

	switch {
	case qerr == nil:
		fmt.Println("VERDICT: NOT REPRODUCED -- appA query still succeeded (no use-after-close)")
	case strings.Contains(strings.ToLower(qerr.Error()), "closed"):
		fmt.Println("VERDICT: REPRODUCED -- appC's Named() evicted appA's live conn; appA hit use-after-close ('closed')")
	default:
		fmt.Printf("VERDICT: appA query failed but NOT with 'closed': %v\n", qerr)
	}
}
