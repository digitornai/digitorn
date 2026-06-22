//go:build onnx

package embeddings_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	bkend "github.com/mbathepaul/digitorn/internal/embeddings/backend"
	"github.com/mbathepaul/digitorn/internal/runtime/context/embeddings"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

func largeUniverse(n int) (univ []policy.AvailableAction, anchors map[string]string) {
	mk := func(mod, act, desc string) policy.AvailableAction {
		return policy.AvailableAction{Module: mod, Action: act, Spec: &tool.Spec{
			Name: mod + "." + act, Description: desc, RiskLevel: tool.RiskLow,
		}}
	}
	univ = []policy.AvailableAction{
		mk("translation", "translate", "Convert written text from one human language into another."),
		mk("weather", "forecast", "Report the expected meteorological conditions for an upcoming day."),
		mk("payment", "refund", "Return funds to a buyer after an order is cancelled."),
	}
	anchors = map[string]string{
		"make this paragraph understandable for a German speaker": "translation.translate",
		"will it be sunny in Lyon this weekend":                   "weather.forecast",
		"give the customer their money back for a returned item":  "payment.refund",
	}

	verbs := []string{"configure", "register", "validate", "compile", "deploy", "monitor", "rotate", "archive", "throttle", "provision"}
	nouns := []string{"certificate", "build cache", "log stream", "feature flag", "load balancer", "cron schedule", "metrics endpoint", "service mesh", "container image", "schema migration"}
	i := 0
	for i < n {
		v := verbs[i%len(verbs)]
		nn := nouns[(i/len(verbs))%len(nouns)]
		mod := fmt.Sprintf("sys%02d", i/10)
		act := fmt.Sprintf("op%02d", i%10)
		desc := fmt.Sprintf("%s the %s for internal infrastructure component number %d.", v, nn, i)
		univ = append(univ, mk(mod, act, desc))
		i++
	}
	return univ, anchors
}

func buildLargeSemanticIndex(t *testing.T, n int) (*index.ToolIndex, map[string]string, time.Duration) {
	t.Helper()
	univ, anchors := largeUniverse(n)
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, univ)

	be, err := bkend.NewONNX(onnxModelDir(t))
	if err != nil {
		t.Fatalf("NewONNX: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	client := onnxClient{be: be}

	start := time.Now()
	si, err := embeddings.NewSemanticIndex(context.Background(), client,
		embeddings.BuildCorpus(idx.Tools))
	embedDur := time.Since(start)
	if err != nil {
		t.Fatalf("NewSemanticIndex: %v", err)
	}
	embeddings.Attach(idx, si, client)
	if idx.Semantic == nil {
		t.Fatal("Attach didn't set Semantic")
	}
	return idx, anchors, embedDur
}

// TestSemanticSearch_Scale_RealModel proves correctness AND measures
// cost at a realistic large corpus : the whole tool index is embedded
// once (the cached first-turn cost), and a no-keyword semantic query
// still pulls its anchor to the top out of 500+ tools.
func TestSemanticSearch_Scale_RealModel(t *testing.T) {
	const n = 500
	idx, anchors, embedDur := buildLargeSemanticIndex(t, n)
	total := len(idx.Tools)
	t.Logf("corpus: %d tools embedded in %s (%.1f ms/tool) — paid ONCE, then cached",
		total, embedDur.Round(time.Millisecond), float64(embedDur.Microseconds())/1000/float64(total))

	for q, want := range anchors {
		start := time.Now()
		hits := idx.Search(q, 5)
		lat := time.Since(start)
		if len(hits) == 0 {
			t.Errorf("[%s] no hits", q)
			continue
		}
		rank := -1
		for i, h := range hits {
			if h.Tool.FQN == want {
				rank = i
				break
			}
		}
		t.Logf("query=%q  top=%s  want=%s  rank=%d  query-latency=%s",
			q, hits[0].Tool.FQN, want, rank, lat.Round(time.Millisecond))
		if rank < 0 || rank > 2 {
			t.Errorf("[%s] %s not in top-3 (rank=%d) among %d tools", q, want, rank, total)
		}
	}
}

// TestSemanticSearch_Concurrency_RealModel fires many parallel queries
// at one shared ONNX-backed index. With `-race` this proves the backend
// session mutex is correct (no data race), nothing panics, and results
// stay correct under contention. Throughput is serialised by the single
// session (one worker) — that is the honest, expected behaviour.
func TestSemanticSearch_Concurrency_RealModel(t *testing.T) {
	idx, anchors, _ := buildLargeSemanticIndex(t, 200)

	// pick one anchor as the hot query
	var q, want string
	for k, v := range anchors {
		q, want = k, v
		break
	}

	const goroutines = 50
	const perG = 20
	var wg sync.WaitGroup
	errs := make(chan string, goroutines*perG)

	start := time.Now()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				hits := idx.Search(q, 3)
				if len(hits) == 0 {
					errs <- "no hits"
					continue
				}
				if hits[0].Tool.FQN != want {
					errs <- fmt.Sprintf("top=%s want=%s", hits[0].Tool.FQN, want)
				}
			}
		}()
	}
	wg.Wait()
	dur := time.Since(start)
	close(errs)

	bad := 0
	var sample string
	for e := range errs {
		bad++
		if sample == "" {
			sample = e
		}
	}
	totalQ := goroutines * perG
	t.Logf("%d concurrent queries in %s (%.0f q/s, single worker) — %d incorrect",
		totalQ, dur.Round(time.Millisecond), float64(totalQ)/dur.Seconds(), bad)
	if bad > 0 {
		t.Errorf("%d/%d concurrent queries returned wrong/empty results (e.g. %s)", bad, totalQ, sample)
	}
}
