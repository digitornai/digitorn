package service

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

// CodecName is the gRPC content-subtype the daemon and workers
// negotiate on. Distinct from llm's "json" so we can evolve them
// independently — e.g. flip module service to msgpack while LLM
// stays on json.
const CodecName = "json+module"

// jsonCodec encodes ModuleService payloads as JSON. Trade-off vs
// protobuf : ~3× slower marshalling but no protoc toolchain and a
// 5-minute on-call developer experience when debugging the wire
// with curl/jq.
//
// The worker pattern is ALREADY behind the in-process fast path
// (see internal/core/servicebus benchmarks : 11.9M ops/sec
// in-process), so JSON's overhead on the worker call path is
// inconsequential relative to the gRPC round-trip.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return CodecName }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}
