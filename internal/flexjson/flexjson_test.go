package flexjson

import (
	"encoding/json"
	"testing"
)

func TestStringSlice_Array(t *testing.T) {
	var p struct {
		Paths StringSlice `json:"paths"`
	}
	if err := json.Unmarshal([]byte(`{"paths":["a.go","b.go"]}`), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Paths) != 2 || p.Paths[0] != "a.go" || p.Paths[1] != "b.go" {
		t.Fatalf("unexpected: %v", p.Paths)
	}
}

func TestStringSlice_SingleString(t *testing.T) {
	var p struct {
		Paths StringSlice `json:"paths"`
	}
	if err := json.Unmarshal([]byte(`{"paths":"a.go"}`), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Paths) != 1 || p.Paths[0] != "a.go" {
		t.Fatalf("unexpected: %v", p.Paths)
	}
}

func TestStringSlice_Null(t *testing.T) {
	var p struct {
		Paths StringSlice `json:"paths"`
	}
	if err := json.Unmarshal([]byte(`{"paths":null}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Paths != nil {
		t.Fatalf("expected nil, got: %v", p.Paths)
	}
}

func TestStringSlice_Empty(t *testing.T) {
	var p struct {
		Paths StringSlice `json:"paths"`
	}
	if err := json.Unmarshal([]byte(`{}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Paths != nil {
		t.Fatalf("expected nil, got: %v", p.Paths)
	}
}

func TestStringSlice_EmptyString(t *testing.T) {
	var p struct {
		Paths StringSlice `json:"paths"`
	}
	if err := json.Unmarshal([]byte(`{"paths":""}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Paths != nil {
		t.Fatalf("expected nil, got: %v", p.Paths)
	}
}

func TestStringSlice_EmptyArray(t *testing.T) {
	var p struct {
		Paths StringSlice `json:"paths"`
	}
	if err := json.Unmarshal([]byte(`{"paths":[]}`), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Paths) != 0 {
		t.Fatalf("expected empty, got: %v", p.Paths)
	}
}
