package indexer

import (
	"strings"
	"testing"
)

func TestKafkaConnector_MessageMapping(t *testing.T) {
	o := parseKafkaOpts(map[string]any{
		"brokers":     []string{"localhost:9092"},
		"topic":       "products",
		"id_field":    "sku",
		"text_fields": []string{"name", "description"},
	})

	// JSON value : id from sku, text from name+description, price → meta.
	doc, del, ok := o.toDoc([]byte("ignored"), []byte(`{"sku":"A1","name":"Widget","description":"a small widget","price":9}`))
	if !ok || del {
		t.Fatalf("json msg: ok=%v del=%v", ok, del)
	}
	if doc.ID != "A1" {
		t.Errorf("id = %q, want A1", doc.ID)
	}
	if !strings.Contains(doc.Text, "Widget") || !strings.Contains(doc.Text, "small widget") {
		t.Errorf("text lost fields: %q", doc.Text)
	}
	if doc.Meta["price"] != "9" {
		t.Errorf("non-text field should be meta: %v", doc.Meta["price"])
	}

	// Tombstone : key + empty value → delete.
	d2, del2, ok2 := o.toDoc([]byte("A1"), nil)
	if !ok2 || !del2 || d2.ID != "A1" {
		t.Errorf("tombstone: ok=%v del=%v id=%q", ok2, del2, d2.ID)
	}

	// Raw (non-JSON) value with a key.
	d3, del3, ok3 := o.toDoc([]byte("K9"), []byte("plain text body"))
	if !ok3 || del3 || d3.ID != "K9" || d3.Text != "plain text body" {
		t.Errorf("raw msg: %+v del=%v ok=%v", d3, del3, ok3)
	}
}
