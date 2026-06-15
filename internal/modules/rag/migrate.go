package rag

import (
	"context"
	"fmt"
)

type MigrateReport struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Model    string `json:"model"`
	Migrated int    `json:"migrated"`
	Dim      int    `json:"dimension"`
}

// Migrate re-embeds every chunk of the src KB with model (a possibly
// different embedding model / dimension) into the dst KB, preserving each
// chunk's id, text, source, position and metadata. Used to move a KB to a
// new model without re-ingesting the original documents. dst defaults to
// "<src>__<model>" ; model defaults to the engine's configured model.
func (e *Engine) Migrate(ctx context.Context, src, dst, model string) (MigrateReport, error) {
	if e.embed == nil {
		return MigrateReport{}, fmt.Errorf("rag: embeddings unavailable (no gateway)")
	}
	if model == "" {
		model = e.model
	}
	if dst == "" {
		dst = src + "__" + pgSafeName.ReplaceAllString(model, "_")
	}
	docs, err := e.backend.Scan(ctx, src)
	if err != nil {
		return MigrateReport{}, fmt.Errorf("rag: scan %q: %w", src, err)
	}
	if len(docs) == 0 {
		return MigrateReport{Source: src, Target: dst, Model: model}, nil
	}

	const batch = 128
	dim := 0
	for i := 0; i < len(docs); i += batch {
		end := i + batch
		if end > len(docs) {
			end = len(docs)
		}
		texts := make([]string, end-i)
		for j := i; j < end; j++ {
			texts[j-i] = docs[j].Text
		}
		vecs, d, err := e.embed.EmbedModel(ctx, model, pkgmoduleRoleDocument, texts)
		if err != nil {
			return MigrateReport{}, fmt.Errorf("rag: re-embed: %w", err)
		}
		if len(vecs) != end-i {
			return MigrateReport{}, fmt.Errorf("rag: re-embed returned %d vectors for %d chunks", len(vecs), end-i)
		}
		dim = d
		if i == 0 {
			if err := e.backend.EnsureKB(ctx, dst, dim); err != nil {
				return MigrateReport{}, fmt.Errorf("rag: ensure target %q: %w", dst, err)
			}
		}
		out := make([]Document, end-i)
		for j := i; j < end; j++ {
			doc := docs[j]
			doc.Vector = vecs[j-i]
			out[j-i] = doc
		}
		if err := e.backend.Upsert(ctx, dst, out); err != nil {
			return MigrateReport{}, fmt.Errorf("rag: upsert target %q: %w", dst, err)
		}
	}
	e.invalidate(dst)
	return MigrateReport{Source: src, Target: dst, Model: model, Migrated: len(docs), Dim: dim}, nil
}
