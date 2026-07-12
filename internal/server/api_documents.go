package server

import (
	"context"
	"os"
	"path/filepath"

	"github.com/digitornai/digitorn/internal/docstore"
)

// seedDocuments binds the app's declared fragmented documents to a session
// workdir: an existing composed file is exploded into its doc-dir, a fresh
// workdir gets the manifest so the very first write is fragment-aware.
// Best-effort — a seeding failure must never block session creation.
func (d *Daemon) seedDocuments(ctx context.Context, appID, wd string) {
	if wd == "" || d.appMgr == nil {
		return
	}
	rt, err := d.appMgr.Get(ctx, appID)
	if err != nil || rt == nil || rt.Definition == nil || len(rt.Definition.Documents) == 0 {
		return
	}
	for _, doc := range rt.Definition.Documents {
		if doc.Seed == "" {
			continue
		}
		composed := filepath.Join(wd, filepath.FromSlash(doc.Seed))
		docDir := composed + docstore.DirSuffix
		if _, err := os.Stat(filepath.Join(docDir, docstore.ManifestFile)); err == nil {
			continue // already bound
		}
		if _, err := os.Stat(composed); err == nil {
			if e := docstore.ExplodeFile(composed, doc.Manifest()); e != nil {
				d.logger.Warn("docstore: explode at bind failed", "app", appID, "doc", doc.Seed, "err", e.Error())
			}
			continue
		}
		if e := docstore.WriteManifest(docDir, doc.Manifest()); e != nil {
			d.logger.Warn("docstore: manifest seed failed", "app", appID, "doc", doc.Seed, "err", e.Error())
		}
	}
}
