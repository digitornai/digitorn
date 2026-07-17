//go:build onnx

package rag

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/indexer"
)

func TestRAG_Enterprise_BigDB_Production(t *testing.T) {
	native := envD("DBACCESS_MYSQL_NATIVE", "root:root@tcp(localhost:3307)/ragtest")
	qurl := envD("QDRANT_URL", "localhost:6334")
	n := envN("ENTERPRISE_ROWS", 10000)

	needles := []struct{ id int; body, query, marker string }{
		{n + 1, "INTERNAL MEMO ROTATION: the production database master credential is rotated on the third Tuesday of each quarter by the platform security team.", "when is the production database master password changed", "ROTATION"},
		{n + 2, "INCIDENT POSTMORTEM FALCON: the third-quarter service interruption was caused by connection pool exhaustion in the billing component after a 02:14 deploy.", "what was the root cause of the Q3 service outage", "FALCON"},
		{n + 3, "PRODUCT SPEC AURORA: the over-ear wireless headset offers multipoint pairing and a low-latency gaming mode at forty milliseconds.", "do the headphones have a mode optimized for playing video games", "AURORA"},
		{n + 4, "HR POLICY ZEPHYR: staff working from home may claim up to eight hundred euros annually for home-office equipment, once per calendar year.", "how much can remote workers expense for their home desk setup", "ZEPHYR"},
		{n + 5, "RUNBOOK ORION: to stop the indexing worker safely, signal a graceful drain, wait for in-flight syncs to finish, then relaunch; never hard-kill during change-data-capture.", "correct procedure to shut down the indexer without losing data", "ORION"},
	}

	t.Logf("=== seeding %d rows of enterprise documents into MySQL ===", n+len(needles))
	seedEnterprise(t, native, n, needles)

	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()
	ret := envD("RETRIEVAL", "hybrid")
	t.Logf("retrieval mode: %s", ret)
	cfg, _ := ParseConfig(map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "qdrant", "url": qurl},
		"pipeline":        map[string]any{"retrieval": ret},
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	_ = be.DeleteKB(context.Background(), "enterprise")
	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	src := SourceConfig{
		Name: "kb", Type: "mysql", KnowledgeBase: "enterprise",
		DSN:         "mysql://root:root@localhost:3307/ragtest",
		Query:       "SELECT id, title, body FROM enterprise_docs ORDER BY id",
		IDColumn:    "id",
		TextColumns: []string{"title", "body"},
	}
	svc := indexer.NewService(nil, 4)

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	start := time.Now()
	rep, err := svc.Sync(context.Background(), genericDBSpec(src, AutoIndex{}), ragSink{eng: eng})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	idxDur := time.Since(start)
	runtime.ReadMemStats(&m1)
	total := n + len(needles)
	if rep.Added != total {
		t.Fatalf("indexed %d, want %d", rep.Added, total)
	}
	dps := float64(total) / idxDur.Seconds()
	t.Logf("INDEX: %d docs in %v → %.0f docs/s | heap Δ=%d MB | extrapolation: 1M rows ≈ %.1f h",
		total, idxDur.Round(time.Millisecond), dps,
		(int64(m1.HeapAlloc)-int64(m0.HeapAlloc))/(1024*1024), 1_000_000/dps/3600)

	found := 0
	for _, nd := range needles {
		hits, err := eng.Query(context.Background(), "enterprise", nd.query, 3)
		if err != nil || len(hits) == 0 {
			t.Errorf("needle %q: no hits (err=%v)", nd.marker, err)
			continue
		}
		ok := false
		for _, h := range hits {
			if strings.Contains(h.Text, nd.marker) {
				ok = true
				break
			}
		}
		if ok {
			found++
		} else {
			t.Logf("needle %q NOT in top-3 for %q (top=%.60q)", nd.marker, nd.query, hits[0].Text)
		}
	}
	t.Logf("CORRECTNESS: %d/%d needles retrieved from a %d-doc haystack by keyword-free semantic query (mode=%s)", found, len(needles), total, ret)
	if found < 3 {
		t.Errorf("retrieval quality too low: only %d/%d needles found", found, len(needles))
	}

	queries := []string{}
	for _, nd := range needles {
		queries = append(queries, nd.query)
	}
	for i := 0; i < 60; i++ {
		queries = append(queries, fmt.Sprintf("information about department %d operations and procedures", i%37))
	}
	var lat []time.Duration
	for _, q := range queries {
		t0 := time.Now()
		if _, err := eng.Query(context.Background(), "enterprise", q, 5); err != nil {
			t.Fatalf("latency query: %v", err)
		}
		lat = append(lat, time.Since(t0))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) time.Duration { return lat[int(float64(len(lat)-1)*q)] }
	t.Logf("LATENCY (%d queries): p50=%v p95=%v p99=%v max=%v",
		len(lat), p(0.50).Round(time.Millisecond), p(0.95).Round(time.Millisecond), p(0.99).Round(time.Millisecond), lat[len(lat)-1].Round(time.Millisecond))

	const conc = 50
	var wg sync.WaitGroup
	var qerr int64
	cstart := time.Now()
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := eng.Query(context.Background(), "enterprise", queries[i%len(queries)], 5); err != nil {
				atomic.AddInt64(&qerr, 1)
			}
		}(i)
	}
	wg.Wait()
	t.Logf("CONCURRENCY: %d parallel queries in %v, errors=%d", conc, time.Since(cstart).Round(time.Millisecond), qerr)
	if qerr != 0 {
		t.Errorf("%d concurrent queries failed", qerr)
	}

	raw, _ := sql.Open("mysql", native)
	defer raw.Close()
	if _, err := raw.Exec("UPDATE enterprise_docs SET body=? WHERE id=?",
		"INCIDENT POSTMORTEM FALCON: REVISED — the outage was actually caused by a expired TLS certificate on the payment gateway, not the connection pool.", needles[1].id); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("DELETE FROM enterprise_docs WHERE id=?", needles[4].id); err != nil {
		t.Fatal(err)
	}
	rep2, err := svc.Sync(context.Background(), genericDBSpec(src, AutoIndex{}), ragSink{eng: eng})
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	t.Logf("INCREMENTAL re-sync: added=%d updated=%d deleted=%d (expected 0/1/1)", rep2.Added, rep2.Updated, rep2.Deleted)
	if rep2.Updated != 1 || rep2.Deleted != 1 {
		t.Errorf("incremental diff wrong: %+v", rep2)
	}
	if hits, _ := eng.Query(context.Background(), "enterprise", "what caused the payment outage tls certificate", 3); len(hits) == 0 || !containsAny(hits, "REVISED", "certificate") {
		t.Errorf("updated FALCON doc not reflected after re-sync")
	}
	if hits, _ := eng.Query(context.Background(), "enterprise", needles[4].query, 3); containsAny(hits, "ORION") {
		t.Errorf("deleted ORION doc still retrievable after re-sync")
	}
	t.Logf("INCREMENTAL: update visible + delete purged ✓")

	_ = be.DeleteKB(context.Background(), "enterprise")
}

func seedEnterprise(t *testing.T, dsn string, n int, needles []struct {
	id           int
	body, query, marker string
}) {
	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	if raw.PingContext(ctx) != nil {
		cancel()
		t.Skipf("no mysql at %s", dsn)
	}
	cancel()
	for _, s := range []string{
		"DROP TABLE IF EXISTS enterprise_docs",
		"CREATE TABLE enterprise_docs (id int PRIMARY KEY, title varchar(160), body text)",
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	kinds := []string{"Support ticket", "Product spec", "Incident report", "HR policy", "Runbook", "Meeting notes", "Customer email", "Release note"}
	depts := []string{"billing", "platform", "security", "logistics", "sales", "support", "data", "mobile"}
	const batch = 1000
	for off := 0; off < n; off += batch {
		var b strings.Builder
		b.WriteString("INSERT INTO enterprise_docs (id,title,body) VALUES ")
		args := []any{}
		count := 0
		for i := off; i < off+batch && i < n; i++ {
			if count > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?,?,?)")
			k := kinds[i%len(kinds)]
			d := depts[(i/3)%len(depts)]
			title := fmt.Sprintf("%s #%d — %s", k, i, d)
			body := fmt.Sprintf("%s for the %s department regarding case %d. The team reviewed the configuration, applied the standard procedure, and confirmed the resolution. Reference number R-%d-%s. Routine operational record, no action required beyond logging.", k, d, i, i, d)
			args = append(args, i, title, body)
			count++
		}
		if count == 0 {
			break
		}
		if _, err := raw.Exec(b.String(), args...); err != nil {
			t.Fatalf("seed batch @%d: %v", off, err)
		}
	}
	for _, nd := range needles {
		if _, err := raw.Exec("INSERT INTO enterprise_docs (id,title,body) VALUES (?,?,?)",
			nd.id, fmt.Sprintf("Knowledge base entry %d", nd.id), nd.body); err != nil {
			t.Fatalf("seed needle: %v", err)
		}
	}
}

func containsAny(hits []SearchHit, subs ...string) bool {
	for _, h := range hits {
		for _, s := range subs {
			if strings.Contains(h.Text, s) {
				return true
			}
		}
	}
	return false
}

func envD(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envN(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
