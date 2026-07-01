package processor

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/channels"
)

func pushProc(ad adapter.Adapter) *ChannelProcessor {
	reg := adapter.NewRegistry()
	reg.Register(ad)
	return &ChannelProcessor{registry: reg, log: discard()}
}

// TestDeliverReply_PushDestination : with a Deliver destination, the message goes
// THERE via that adapter — even though the inbound event came from another transport
// (webhook) with no reply handle. This is the proactive-push routing.
func TestDeliverReply_PushDestination(t *testing.T) {
	ad := &fakeAdapter{} // registers under "fake"
	p := pushProc(ad)

	ev := adapter.Event{Adapter: "webhook"} // a push trigger: no ReplyRef
	act := channels.Activation{
		Deliver: &channels.Destination{Adapter: "fake", Ref: map[string]any{"provider": "dc", "channel_id": "42"}},
	}
	p.deliverReply(context.Background(), ev, act, TriggerSpec{}, "Build #7 ✅")

	if len(ad.sent) != 1 || ad.sent[0] != "Build #7 ✅" {
		t.Fatalf("push not delivered: %+v", ad.sent)
	}
	if ad.toRef[0]["channel_id"] != "42" {
		t.Fatalf("wrong destination ref: %+v", ad.toRef[0])
	}
}

// TestDeliverReply_ReactiveFallback : with no Deliver, the message goes back to the
// inbound originator (event adapter + reply handle) — unchanged behaviour.
func TestDeliverReply_ReactiveFallback(t *testing.T) {
	ad := &fakeAdapter{}
	p := pushProc(ad)

	ev := adapter.Event{Adapter: "fake", ReplyRef: map[string]any{"channel_id": "99"}}
	p.deliverReply(context.Background(), ev, channels.Activation{}, TriggerSpec{}, "hi back")

	if len(ad.sent) != 1 || ad.sent[0] != "hi back" || ad.toRef[0]["channel_id"] != "99" {
		t.Fatalf("reactive reply wrong: sent=%+v ref=%+v", ad.sent, ad.toRef)
	}
}

// TestDeliverReply_NoTargetNoSend : no Deliver and no inbound reply handle → nothing
// is sent (a misconfigured/absent destination must not panic or mis-route).
func TestDeliverReply_NoTargetNoSend(t *testing.T) {
	ad := &fakeAdapter{}
	p := pushProc(ad)
	p.deliverReply(context.Background(), adapter.Event{Adapter: "webhook"}, channels.Activation{}, TriggerSpec{}, "orphan")
	if len(ad.sent) != 0 {
		t.Fatalf("nothing should be sent without a destination: %+v", ad.sent)
	}
}
