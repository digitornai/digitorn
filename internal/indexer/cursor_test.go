package indexer

import "testing"

func TestFileCursor_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	c1, err := NewFileCursor(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "code\x00web\x00site"
	if err := c1.Save(key, []byte("0/16ABCD")); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A fresh instance (simulating a worker restart) must read it back.
	c2, err := NewFileCursor(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c2.Load(key)
	if err != nil || string(got) != "0/16ABCD" {
		t.Errorf("persisted = %q err=%v, want 0/16ABCD", got, err)
	}

	// Missing key → nil, no error.
	if b, err := c2.Load("never-saved"); b != nil || err != nil {
		t.Errorf("missing key load = %q err=%v, want nil/nil", b, err)
	}

	// Overwrite is atomic + visible.
	_ = c1.Save(key, []byte("0/FF0000"))
	if got, _ := c2.Load(key); string(got) != "0/FF0000" {
		t.Errorf("overwrite not visible: %q", got)
	}
}
