package channels

import (
	"context"
	"testing"
)

// TestResolveDeliverTemplated : a push destination is resolved with the adapter name
// and every ref value rendered over the event scope (so a webhook payload can target
// a specific channel).
func TestResolveDeliverTemplated(t *testing.T) {
	ev := Event{Provider: "ci", Adapter: "webhook", Payload: map[string]any{"channel": "999"}}
	ac := ActivationConfig{
		Message: "hi",
		Reply:   ReplyNone,
		Deliver: &DeliverConfig{
			Adapter: "discord",
			Ref:     map[string]string{"provider": "dc", "channel_id": "{{event.payload.channel}}"},
		},
	}
	act, err := Process(context.Background(), ev, ac, ModuleConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if act.Deliver == nil {
		t.Fatal("deliver must be resolved")
	}
	if act.Deliver.Adapter != "discord" {
		t.Fatalf("adapter = %q", act.Deliver.Adapter)
	}
	if act.Deliver.Ref["provider"] != "dc" {
		t.Fatalf("provider = %v", act.Deliver.Ref["provider"])
	}
	if act.Deliver.Ref["channel_id"] != "999" {
		t.Fatalf("channel_id = %v (template not rendered)", act.Deliver.Ref["channel_id"])
	}
}

// TestResolveDeliverNilWhenUnset : no deliver block → reactive (Deliver nil).
func TestResolveDeliverNilWhenUnset(t *testing.T) {
	act, err := Process(context.Background(), Event{}, ActivationConfig{Message: "x"}, ModuleConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if act.Deliver != nil {
		t.Fatal("deliver must be nil when unset")
	}
}

// TestDeliverValidation : a deliver block requires an adapter and a non-empty ref.
func TestDeliverValidation(t *testing.T) {
	validate := func(d *DeliverConfig) error {
		m := ModuleConfig{Providers: map[string]ProviderConfig{
			"p": {Adapter: "webhook", Activation: ActivationConfig{Deliver: d}},
		}}
		m.Normalize() // fill bounds defaults so Validate only trips on deliver
		return m.Validate()
	}
	if err := validate(&DeliverConfig{Ref: map[string]string{"x": "y"}}); err == nil {
		t.Fatal("missing deliver.adapter must fail validation")
	}
	if err := validate(&DeliverConfig{Adapter: "discord"}); err == nil {
		t.Fatal("missing deliver.ref must fail validation")
	}
	if err := validate(&DeliverConfig{Adapter: "discord", Ref: map[string]string{"channel_id": "1"}}); err != nil {
		t.Fatalf("valid deliver must pass: %v", err)
	}
}
