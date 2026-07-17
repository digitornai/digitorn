package indexer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

func TestKafkaConnector_Live(t *testing.T) {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("set KAFKA_BROKERS to run the live Kafka test")
	}
	bl := strings.Split(brokers, ",")
	topic := "rag_test_products"
	ctx := context.Background()

	w := &kafka.Writer{Addr: kafka.TCP(bl...), Topic: topic, AllowAutoTopicCreation: true, Balancer: &kafka.LeastBytes{}}
	defer w.Close()
	if err := w.WriteMessages(ctx, kafka.Message{Key: []byte("seed"), Value: []byte(`{"sku":"seed","name":"seed"}`)}); err != nil {
		t.Fatalf("produce seed: %v", err)
	}

	conn := &kafkaConnector{}
	spec := SourceSpec{Name: "k", Type: "kafka", KB: "kb", Opts: map[string]any{
		"brokers": bl, "topic": topic, "group_id": "rag_test_grp", "id_field": "sku", "text_fields": []string{"name"},
	}}
	sink := newFakeSink()
	wctx, cancel := context.WithCancel(ctx)
	go func() { _ = conn.Watch(wctx, spec, sink, NewMemCursor()) }()
	defer cancel()

	time.Sleep(4 * time.Second)

	if err := w.WriteMessages(ctx, kafka.Message{Key: []byte("A1"), Value: []byte(`{"sku":"A1","name":"alpha widget"}`)}); err != nil {
		t.Fatalf("produce: %v", err)
	}
	if !waitFor(func() bool { return strings.Contains(sink.text("A1"), "alpha widget") }, 25*time.Second) {
		t.Fatal("kafka message not consumed into the sink")
	}
	t.Logf("kafka upsert received: %q", sink.text("A1"))

	if err := w.WriteMessages(ctx, kafka.Message{Key: []byte("A1"), Value: nil}); err != nil {
		t.Fatalf("produce tombstone: %v", err)
	}
	if !waitFor(func() bool { return sink.wasDeleted("A1") }, 25*time.Second) {
		t.Fatal("kafka tombstone not consumed as a delete")
	}
	t.Log("kafka delete (tombstone) received")
}
