package contextsvc

import (
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func smallBrain() schema.Brain {
	return schema.Brain{
		Provider: "openai",
		Model:    "gpt-4o",
		Context:  &schema.ContextConfig{MaxTokens: 10000, OutputReserved: 2000},
	}
}

func TestViewFromSnapshot_FullVariable(t *testing.T) {
	snap := sessionstore.SessionSnapshot{
		ContextTokens:        4000,
		ContextSystemTokens:  1000,
		ContextToolsTokens:   1000,
		ContextMessageTokens: 2000,
		TurnCount:            3,
		TokensIn:             5000,
		TokensOut:            1200,
		UsdTotal:             0.04,
		LastSeq:              42,
	}
	v := ViewFromSnapshot(snap, smallBrain())

	if v.Window != 10000 || v.Limit != 8000 || v.OutputReserved != 2000 {
		t.Fatalf("window/limit/reserved = %d/%d/%d, want 10000/8000/2000", v.Window, v.Limit, v.OutputReserved)
	}
	if v.Used != 4000 || v.Remaining != 4000 {
		t.Errorf("used/remaining = %d/%d, want 4000/4000", v.Used, v.Remaining)
	}
	if v.Pressure != 0.5 || v.PressurePct != 50 {
		t.Errorf("pressure = %v (%d%%), want 0.5 (50%%)", v.Pressure, v.PressurePct)
	}
	if v.System != 1000 || v.Tools != 1000 || v.Messages != 2000 {
		t.Errorf("breakdown = %d/%d/%d, want 1000/1000/2000", v.System, v.Tools, v.Messages)
	}
	if v.SystemPct != 25 || v.MessagesPct != 50 {
		t.Errorf("pct = sys %d msgs %d, want 25 / 50", v.SystemPct, v.MessagesPct)
	}
	if v.Turns != 3 || v.TokensIn != 5000 || v.TokensOut != 1200 || v.CostUSD != 0.04 {
		t.Errorf("conv/cost mismatch: turns=%d in=%d out=%d usd=%v", v.Turns, v.TokensIn, v.TokensOut, v.CostUSD)
	}
	if v.Provider != "openai" || v.Model != "gpt-4o" || v.UpdatedSeq != 42 {
		t.Errorf("model/freshness mismatch: %s/%s seq=%d", v.Provider, v.Model, v.UpdatedSeq)
	}
	if v.Source != "anchor" || !v.HasAnchor {
		t.Errorf("source/anchor = %q/%v, want anchor/true", v.Source, v.HasAnchor)
	}
}

func TestViewWithExactTotal_OverlaysAndRederives(t *testing.T) {
	base := ViewFromSnapshot(sessionstore.SessionSnapshot{ContextTokens: 100}, smallBrain())
	got := base.WithExactTotal(6000, 1500, 1500, 3000)
	if got.Used != 6000 || got.System != 1500 || got.Messages != 3000 {
		t.Fatalf("overlay failed: used=%d sys=%d msgs=%d", got.Used, got.System, got.Messages)
	}
	if got.Source != "tokenizer" || !got.Exact {
		t.Errorf("source/exact = %q/%v, want tokenizer/true", got.Source, got.Exact)
	}
	if got.PressurePct != 75 || got.Remaining != 2000 {
		t.Errorf("re-derive: pressure %d%% remaining %d, want 75%% / 2000", got.PressurePct, got.Remaining)
	}
}

func TestTracker_PutGetDelete_Isolated(t *testing.T) {
	tr := NewTracker()
	if _, ok := tr.Get("s1"); ok {
		t.Fatal("empty tracker returned a view")
	}
	tr.Put("s1", ContextView{Used: 111})
	tr.Put("s2", ContextView{Used: 222})
	if v, _ := tr.Get("s1"); v.Used != 111 {
		t.Errorf("s1 = %d, want 111", v.Used)
	}
	if v, _ := tr.Get("s2"); v.Used != 222 {
		t.Errorf("s2 = %d, want 222 (cross-session leak?)", v.Used)
	}
	tr.Delete("s1")
	if _, ok := tr.Get("s1"); ok {
		t.Error("s1 still present after delete")
	}
	if _, ok := tr.Get("s2"); !ok {
		t.Error("delete of s1 dropped s2")
	}
}

func TestTracker_ConcurrentPutGet(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sid := "s"
			tr.Put(sid, ContextView{Used: n})
			_, _ = tr.Get(sid)
		}(i)
	}
	wg.Wait()
	if _, ok := tr.Get("s"); !ok {
		t.Error("expected a view after concurrent writes")
	}
}
