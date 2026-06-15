package rag

import (
	"context"
	"testing"

	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

// The indexer scheduler runs syncFn with context.Background() (no UserID/AppID).
// With ACL enabled (scope=user, default), docMeta stamps NO owner, so every
// user's owner-filter excludes the doc: trigger-indexed source docs are invisible.
func TestProbe_ACL_IndexerContext_DocsInvisible(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{"acl": map[string]any{"enabled": true}})
	eng := NewEngine(cfg, newFakeBackend(), fakeEmbedder{dim: 64}, nil)

	// Simulate the indexer sink: ingest with a bare Background context (what
	// scheduler.runJob passes). No UserID, no AppID.
	bg := context.Background()
	if _, err := eng.IngestWithMeta(bg, "kb", "indexed source document about deploy application production server", "source.md", map[string]any{"url": "https://x"}); err != nil {
		t.Fatalf("indexer-style ingest: %v", err)
	}

	// A real user queries. Under ACL the filter is owner==alice; the doc has no
	// owner, so it must be filtered out.
	alice := pkgmodule.WithUserID(context.Background(), "alice")
	hits, err := eng.Query(alice, "kb", "deploy application server", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	t.Logf("ACL+indexer: alice sees %d hits for an indexer-ingested doc (0 = INVISIBLE, the gap)", len(hits))
	if len(hits) == 0 {
		t.Logf(">>> ACL+SOURCES GAP: trigger-indexed docs (owner-less Background ctx) are unretrievable for EVERY user when ACL is on.")
	}

	// Now an agent (alice) triggers a manual reindex of the same content: her
	// id gets stamped -> the doc becomes HERS, not a shared/app resource.
	if _, err := eng.IngestWithMeta(alice, "kb", "indexed source document about deploy application production server", "source.md", map[string]any{"url": "https://x"}); err != nil {
		t.Fatalf("manual reindex: %v", err)
	}
	aHits, _ := eng.Query(alice, "kb", "deploy application server", 10)
	bob := pkgmodule.WithUserID(context.Background(), "bob")
	bHits, _ := eng.Query(bob, "kb", "deploy application server", 10)
	t.Logf("after alice manual reindex: alice sees %d, bob sees %d (alice>0 & bob=0 => source doc became privately owned by the reindexing agent)", len(aHits), len(bHits))
}
