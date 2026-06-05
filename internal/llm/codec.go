package llm

import (
	"github.com/bytedance/sonic"

	"google.golang.org/grpc/encoding"
)

// jsonCodec is registered globally with gRPC so our LLM service can carry
// Go-native types without requiring protoc.
//
// Phase 7: switched encoding/json → bytedance/sonic. Sonic is a JIT-
// compiled JSON encoder with full encoding/json API parity for the
// shapes we use (struct tags, omitempty, nested slices/maps). Drop-in
// replacement, ~3-5× faster on the typical request/response sizes.
// Wire format unchanged (still UTF-8 JSON), so clients/servers across
// the daemon↔worker boundary stay interoperable on rolling upgrades.
//
// Migration to protobuf is still a non-breaking change — only the codec
// registration changes; the public types in types.go stay identical.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error) { return sonic.Marshal(v) }

func (jsonCodec) Unmarshal(data []byte, v any) error { return sonic.Unmarshal(data, v) }

func (jsonCodec) Name() string { return CodecName }

// CodecName is the content-subtype identifier passed on the gRPC wire so
// servers and clients pick the same encoding.
const CodecName = "json"

func init() {
	encoding.RegisterCodec(jsonCodec{})
}
