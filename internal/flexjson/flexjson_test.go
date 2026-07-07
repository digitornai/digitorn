package flexjson

import (
	"encoding/json"
	"testing"
)

func TestRawArray_Native(t *testing.T) {
	var p struct {
		Patch RawArray `json:"patch"`
	}
	if err := json.Unmarshal([]byte(`{"patch":[{"op":"replace","path":"/x","value":1}]}`), &p); err != nil {
		t.Fatal(err)
	}
	if string(p.Patch) != `[{"op":"replace","path":"/x","value":1}]` {
		t.Fatalf("unexpected: %s", p.Patch)
	}
}

func TestRawArray_DoubleEncodedString(t *testing.T) {
	// Some models emit an array-typed argument as an escaped JSON string
	// instead of native JSON — this is the exact shape that produced
	// "cannot unmarshal string into Go value of type jsonpatch.Patch".
	var p struct {
		Patch RawArray `json:"patch"`
	}
	raw := []byte(`{"patch":"[{\"op\":\"replace\",\"path\":\"/x\",\"value\":1}]"}`)
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if string(p.Patch) != `[{"op":"replace","path":"/x","value":1}]` {
		t.Fatalf("unexpected: %s", p.Patch)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(p.Patch, &decoded); err != nil {
		t.Fatalf("resulting bytes are not valid JSON: %v", err)
	}
}

func TestRawArray_Empty(t *testing.T) {
	var p struct {
		Patch RawArray `json:"patch"`
	}
	if err := json.Unmarshal([]byte(`{"patch":null}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Patch != nil {
		t.Fatalf("expected nil, got %s", p.Patch)
	}
	if err := json.Unmarshal([]byte(`{}`), &p); err != nil {
		t.Fatal(err)
	}
}

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
