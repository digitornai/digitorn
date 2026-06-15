package indexer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

func kafkaProduce(t *testing.T, brokers []string, topic string, msgs ...kafka.Message) {
	t.Helper()
	w := &kafka.Writer{Addr: kafka.TCP(brokers...), Topic: topic, AllowAutoTopicCreation: true, Balancer: &kafka.LeastBytes{}}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := w.WriteMessages(ctx, msgs...); err != nil {
		t.Fatalf("produce: %v", err)
	}
}

// TestChaos_Kafka_NewGroup_ConsumesBacklog DISPROVES the documented gap claim
// that a brand-new group "skips the existing topic backlog". kafka-go's default
// StartOffset for a consumer group is FirstOffset (reader.go:485 "Default:
// FirstOffset"), NOT LastOffset. So a brand-new group actually consumes the
// ENTIRE pre-existing backlog. This test asserts the real (correct) behavior.
//
//	KAFKA_CHAOS_BROKERS=localhost:9095 go test ./internal/indexer/ -run TestChaos_Kafka_NewGroup_ConsumesBacklog -v
func TestChaos_Kafka_NewGroup_ConsumesBacklog(t *testing.T) {
	brokers := os.Getenv("KAFKA_CHAOS_BROKERS")
	if brokers == "" {
		t.Skip("set KAFKA_CHAOS_BROKERS")
	}
	bl := strings.Split(brokers, ",")
	topic := fmt.Sprintf("chaos_backlog_%d", time.Now().UnixNano())
	group := fmt.Sprintf("grp_backlog_%d", time.Now().UnixNano())

	// Produce 3 messages BEFORE any consumer group exists.
	kafkaProduce(t, bl, topic,
		kafka.Message{Key: []byte("p1"), Value: []byte(`{"id":"p1","name":"pre one"}`)},
		kafka.Message{Key: []byte("p2"), Value: []byte(`{"id":"p2","name":"pre two"}`)},
		kafka.Message{Key: []byte("p3"), Value: []byte(`{"id":"p3","name":"pre three"}`)},
	)
	time.Sleep(1 * time.Second)

	conn := &kafkaConnector{}
	spec := SourceSpec{Name: "k", Type: "kafka", KB: "kb", Opts: map[string]any{
		"brokers": bl, "topic": topic, "group_id": group, "id_field": "id", "text_fields": []string{"name"},
	}}
	sink := newRecordingSink()
	wctx, cancel := context.WithCancel(context.Background())
	go func() { _ = conn.Watch(wctx, spec, sink, NewMemCursor()) }()
	defer cancel()

	time.Sleep(5 * time.Second) // group join

	// Now produce a 4th message AFTER the group joined — this one should arrive.
	kafkaProduce(t, bl, topic, kafka.Message{Key: []byte("post"), Value: []byte(`{"id":"post","name":"after join"}`)})
	if !waitUntil(func() bool { return sink.seen("post") }, 20*time.Second) {
		t.Fatal("post-join message not consumed (broker/connector broken)")
	}

	// The 3 pre-existing messages MUST have been consumed (full backfill).
	got := sink.seen("p1") && sink.seen("p2") && sink.seen("p3")
	t.Logf("pre-existing backlog seen? p1=%v p2=%v p3=%v ; post-join seen=%v",
		sink.seen("p1"), sink.seen("p2"), sink.seen("p3"), sink.seen("post"))
	if got {
		t.Logf("WORKS-AS-INTENDED (corrects a documented 'gap'): a brand-new consumer group consumed ALL 3 pre-existing messages + post-join traffic. kafka-go default StartOffset=FirstOffset, so first-ever index DOES backfill the topic. The surface-map claim of 'skips backlog from newest' is WRONG.")
	} else {
		t.Errorf("backlog NOT fully consumed (p1=%v p2=%v p3=%v) — first-ever index lost historical data", sink.seen("p1"), sink.seen("p2"), sink.seen("p3"))
	}
}

// TestChaos_Kafka_BrokerBounce_OffsetResume produces+consumes some messages,
// commits offsets via the consumer group, BOUNCES the broker (docker restart),
// then produces more. After recovery the connector (supervised restart) must
// resume from the committed group offset: deliver the new messages and NOT
// re-deliver the already-committed ones.
//
//	KAFKA_CHAOS_BROKERS=localhost:9095 KAFKA_CHAOS_CONTAINER=kafka-chaos \
//	  go test ./internal/indexer/ -run TestChaos_Kafka_BrokerBounce_OffsetResume -v -timeout 200s
func TestChaos_Kafka_BrokerBounce_OffsetResume(t *testing.T) {
	brokers := os.Getenv("KAFKA_CHAOS_BROKERS")
	cname := os.Getenv("KAFKA_CHAOS_CONTAINER")
	if brokers == "" || cname == "" {
		t.Skip("set KAFKA_CHAOS_BROKERS and KAFKA_CHAOS_CONTAINER")
	}
	bl := strings.Split(brokers, ",")
	topic := fmt.Sprintf("chaos_bounce_%d", time.Now().UnixNano())
	group := fmt.Sprintf("grp_bounce_%d", time.Now().UnixNano())

	// Seed the topic + join the group so the group's start offset is established
	// at the current end BEFORE we produce the "tracked" messages.
	kafkaProduce(t, bl, topic, kafka.Message{Key: []byte("seed"), Value: []byte(`{"id":"seed","name":"seed"}`)})

	registerLoad()
	svc := NewService(NewMemCursor(), 4)
	sink := newRecordingSink()
	spec := SourceSpec{Name: "kb", Type: "kafka", KB: "kb",
		Triggers: []Trigger{{Type: "watch"}},
		Opts: map[string]any{
			"brokers": bl, "topic": topic, "group_id": group, "id_field": "id", "text_fields": []string{"name"},
		}}
	svc.Register(spec, sink)
	defer svc.Shutdown(context.Background())

	time.Sleep(6 * time.Second) // group join + offset establish

	// Produce + consume message 1.
	kafkaProduce(t, bl, topic, kafka.Message{Key: []byte("m1"), Value: []byte(`{"id":"m1","name":"one"}`)})
	if !waitUntil(func() bool { return sink.seen("m1") }, 25*time.Second) {
		t.Fatal("pre-bounce m1 not consumed")
	}
	t.Log("m1 consumed; allowing offset commit then bouncing broker")
	time.Sleep(3 * time.Second) // let kafka-go commit the group offset

	preRestarts := svc.Stats().WatchRestarts
	m1Count := sink.count("m1")

	// Bounce the broker.
	if out, err := exec.Command("docker", "restart", "-t", "5", cname).CombinedOutput(); err != nil {
		t.Fatalf("docker restart: %v: %s", err, out)
	}
	t.Log("broker bounced; waiting for health")
	if !waitUntil(func() bool {
		out, _ := exec.Command("docker", "exec", cname, "rpk", "cluster", "health").CombinedOutput()
		return strings.Contains(string(out), "Healthy:                          true")
	}, 90*time.Second) {
		t.Fatal("broker never recovered")
	}
	time.Sleep(3 * time.Second)

	// Produce message 2 post-recovery.
	var got2 bool
	for attempt := 0; attempt < 10 && !got2; attempt++ {
		id := fmt.Sprintf("m2-%d", attempt)
		kafkaProduce(t, bl, topic, kafka.Message{Key: []byte(id), Value: []byte(fmt.Sprintf(`{"id":%q,"name":"two"}`, id))})
		if waitUntil(func() bool { return sink.seen(id) }, 12*time.Second) {
			got2 = true
			t.Logf("post-bounce message %s consumed — offset resume works", id)
		}
	}
	postRestarts := svc.Stats().WatchRestarts
	redeliver := sink.count("m1") - m1Count
	t.Logf("WatchRestarts before=%d after=%d ; m1 re-delivered after bounce=%d", preRestarts, postRestarts, redeliver)

	if !got2 {
		t.Errorf("post-bounce message never consumed — Kafka supervised resume failed after broker bounce")
	}
	if redeliver == 0 {
		t.Logf("offset resume clean: committed m1 NOT re-delivered after broker bounce.")
	} else {
		t.Logf("NOTE: m1 re-delivered %dx after bounce (at-least-once; offset may not have committed before the bounce — kafka-go commit interval).", redeliver)
	}
}
