package blobstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
)

// BenchmarkPut_1MB measures the latency of writing a 1 MB blob from
// scratch (no dedup hit). Reference target : ≤ 5ms on typical SSD.
func BenchmarkPut_1MB(b *testing.B) {
	dir := b.TempDir()
	body := make([]byte, 1<<20)
	_, _ = rand.Read(body)
	s := New(dir)
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		// Mutate one byte each iteration so we hit the write path, not
		// the dedup short-circuit.
		body[0] = byte(i)
		if _, err := s.Put(ctx, "application/octet-stream", bytes.NewReader(body)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPut_Dedup measures the latency of Put when the bytes are
// already on disk (the hot path for image uploads in shared rooms).
// Reference target : ≤ 1ms — just hash + stat.
func BenchmarkPut_Dedup(b *testing.B) {
	dir := b.TempDir()
	body := make([]byte, 1<<20)
	_, _ = rand.Read(body)
	s := New(dir)
	ctx := context.Background()
	// Prime the blob so all subsequent Puts dedup.
	if _, err := s.Put(ctx, "application/octet-stream", bytes.NewReader(body)); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, err := s.Put(ctx, "application/octet-stream", bytes.NewReader(body)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGet_1MB measures the latency of reading back a 1 MB blob.
// Reference target : ≤ 1ms once the FS cache is warm.
func BenchmarkGet_1MB(b *testing.B) {
	dir := b.TempDir()
	body := make([]byte, 1<<20)
	_, _ = rand.Read(body)
	s := New(dir)
	ctx := context.Background()
	ref, err := s.Put(ctx, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		rc, err := s.Get(ctx, ref.Hash)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
}
