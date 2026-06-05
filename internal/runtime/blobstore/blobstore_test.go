package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPut_HashMatchesSha256OfBody(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	body := []byte("hello world")
	want := sha256.Sum256(body)

	ref, err := s.Put(context.Background(), "text/plain", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash != hex.EncodeToString(want[:]) {
		t.Fatalf("hash mismatch\n  got  %s\n  want %s", ref.Hash, hex.EncodeToString(want[:]))
	}
	if ref.Size != int64(len(body)) {
		t.Fatalf("size mismatch : got %d want %d", ref.Size, len(body))
	}
	if ref.Mime != "text/plain" {
		t.Fatalf("mime not preserved : got %q", ref.Mime)
	}
}

func TestGet_RoundTripsBytesExactly(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	body := []byte("\x00\x01binary\x02\x03 payload \xff\xfe")

	ref, err := s.Put(context.Background(), "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(context.Background(), ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("bytes mismatch\n  got  %v\n  want %v", got, body)
	}
}

func TestPut_DedupsIdenticalBytes(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	body := []byte("same payload")

	ref1, err := s.Put(context.Background(), "text/plain", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := s.Put(context.Background(), "text/plain", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ref1.Hash != ref2.Hash {
		t.Fatalf("dedup broke : same bytes produced different hashes\n  %s\n  %s", ref1.Hash, ref2.Hash)
	}
	// Count files on disk : only ONE blob should exist regardless of how
	// many times the same bytes were Put.
	count := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			count++
		}
		return nil
	})
	if count != 1 {
		t.Fatalf("expected exactly 1 file on disk after dedup, got %d", count)
	}
}

func TestPut_ConcurrentSameHash_NoCorruption(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	body := []byte("concurrent payload")
	const N = 16

	var wg sync.WaitGroup
	refs := make([]string, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref, err := s.Put(context.Background(), "text/plain", bytes.NewReader(body))
			refs[i] = ref.Hash
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d : %v", i, err)
		}
	}
	for i := 1; i < N; i++ {
		if refs[i] != refs[0] {
			t.Fatalf("concurrent Put produced different hashes : %s vs %s", refs[i], refs[0])
		}
	}
	// Bytes integrity : the final blob on disk must equal the body.
	rc, err := s.Get(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatal("concurrent Put corrupted the blob bytes")
	}
}

func TestGet_NotFound(t *testing.T) {
	s := New(t.TempDir())
	fakeHash := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := s.Get(context.Background(), fakeHash)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGet_InvalidHashRejected(t *testing.T) {
	s := New(t.TempDir())
	bad := []string{
		"",
		"not-a-hash",
		"../../etc/passwd",
		"AAAA",                             // too short
		"GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG", // 32 chars but non-hex AND wrong len
	}
	for _, h := range bad {
		if _, err := s.Get(context.Background(), h); err == nil {
			t.Fatalf("Get must reject invalid hash %q", h)
		}
	}
}

func TestDelete_IdempotentOnMissing(t *testing.T) {
	s := New(t.TempDir())
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := s.Delete(context.Background(), hash); err != nil {
		t.Fatalf("Delete on missing must be idempotent : %v", err)
	}
}

func TestDelete_RemovesFromDisk(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ref, _ := s.Put(context.Background(), "text/plain", bytes.NewReader([]byte("trash")))
	if !s.Exists(ref.Hash) {
		t.Fatal("blob missing right after Put")
	}
	if err := s.Delete(context.Background(), ref.Hash); err != nil {
		t.Fatal(err)
	}
	if s.Exists(ref.Hash) {
		t.Fatal("Delete left the blob on disk")
	}
}

func TestPut_CtxCancelAborts(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	_, err := s.Put(ctx, "text/plain", bytes.NewReader([]byte("payload")))
	if err == nil {
		t.Fatal("Put with cancelled ctx must error")
	}
	// And no garbage file should remain in the root.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Fatalf("Put left a file behind on ctx cancel : %s", e.Name())
	}
}

func TestPut_LargeBlobStreamsHash(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	// 4 MB of pseudo-random bytes ; if hashing were buffered fully in
	// memory this would still work but the test guarantees the path
	// can handle a non-trivial size.
	body := make([]byte, 4<<20)
	for i := range body {
		body[i] = byte(i * 31)
	}
	wantHash := sha256.Sum256(body)
	ref, err := s.Put(context.Background(), "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash != hex.EncodeToString(wantHash[:]) {
		t.Fatal("large blob hash mismatch")
	}
	rc, _ := s.Get(context.Background(), ref.Hash)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatal("large blob bytes mismatch")
	}
}
