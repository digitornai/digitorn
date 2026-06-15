package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	kafka "github.com/segmentio/kafka-go"
)

func init() { Register(&kafkaConnector{}) }

// kafkaConnector consumes a Kafka topic as a continuous change stream
// (Watch only) : each message → one document (id from the message key or a
// JSON field, text from configured fields, the rest as metadata), an empty
// value (tombstone) → delete. Offsets are committed to the consumer group,
// so a restart resumes exactly where it left off.
type kafkaConnector struct{}

func (*kafkaConnector) Type() string                                          { return "kafka" }
func (*kafkaConnector) Capabilities() Caps                                     { return Caps{Watch: true} }
func (*kafkaConnector) Walk(context.Context, SourceSpec, func(Document) error) error { return nil }

type kafkaOpts struct {
	Brokers    []string
	Topic      string
	Group      string
	IDField    string
	TextFields map[string]bool
}

func parseKafkaOpts(opts map[string]any) kafkaOpts {
	o := kafkaOpts{TextFields: map[string]bool{}}
	o.Brokers = optStrings(opts, "brokers")
	o.Topic = optString(opts, "topic")
	o.Group = optString(opts, "group_id")
	if o.Group == "" {
		o.Group = "digitorn-indexer"
	}
	o.IDField = optString(opts, "id_field")
	for _, f := range optStrings(opts, "text_fields") {
		o.TextFields[f] = true
	}
	return o
}

func (*kafkaConnector) Watch(ctx context.Context, spec SourceSpec, sink Sink, _ Cursor) error {
	o := parseKafkaOpts(spec.Opts)
	if len(o.Brokers) == 0 || o.Topic == "" {
		return fmt.Errorf("indexer/kafka: brokers and topic required")
	}
	r := kafka.NewReader(kafka.ReaderConfig{Brokers: o.Brokers, Topic: o.Topic, GroupID: o.Group})
	defer r.Close()
	for {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("indexer/kafka: read: %w", err)
		}
		doc, del, ok := o.toDoc(m.Key, m.Value)
		if !ok {
			continue
		}
		if del {
			_ = sink.Delete(ctx, spec.KB, doc.ID)
		} else {
			_ = sink.Upsert(ctx, spec.KB, []Document{doc})
		}
	}
}

// toDoc maps a Kafka (key,value) to a document. Returns (doc, isDelete, ok).
// Empty value with a key = a compaction tombstone → delete.
func (o kafkaOpts) toDoc(key, value []byte) (Document, bool, bool) {
	id := string(key)
	if len(value) == 0 {
		if id == "" {
			return Document{}, false, false
		}
		return Document{ID: id}, true, true
	}

	var obj map[string]any
	if json.Unmarshal(value, &obj) == nil && len(obj) > 0 {
		if o.IDField != "" {
			if v, ok := obj[o.IDField]; ok {
				id = anyToStr(v)
			}
		}
		if id == "" {
			return Document{}, false, false
		}
		var parts []string
		meta := map[string]any{}
		for k, v := range obj {
			if k == o.IDField {
				continue
			}
			if len(o.TextFields) == 0 || o.TextFields[k] {
				if s := anyToStr(v); s != "" {
					parts = append(parts, s)
				}
			} else {
				meta[k] = anyToStr(v)
			}
		}
		return Document{ID: id, Text: strings.Join(parts, "\n"), Meta: meta}, false, true
	}

	if id == "" {
		return Document{}, false, false
	}
	return Document{ID: id, Text: string(value)}, false, true
}

func anyToStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}
