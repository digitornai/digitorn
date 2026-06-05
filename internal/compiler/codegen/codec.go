package codegen

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func Build(def *schema.AppDefinition, cat *catalog.Catalog) (*Artifact, error) {
	hash, err := VersionHash(def, cat)
	if err != nil {
		return nil, fmt.Errorf("codegen: hash: %w", err)
	}
	a := &Artifact{
		Header: Header{
			Magic:           [4]byte{FileMagic[0], FileMagic[1], FileMagic[2], FileMagic[3]},
			Format:          FormatVersion,
			CompilerVersion: CompilerVersion,
			CompiledAt:      time.Now().UTC().Unix(),
			VersionHash:     hash,
		},
		Definition: def,
	}
	return a, nil
}

func Encode(w io.Writer, a *Artifact) error {
	if _, err := w.Write(a.Header.Magic[:]); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, a.Header.Format); err != nil {
		return err
	}
	if err := writeLenString(w, a.Header.CompilerVersion); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, a.Header.CompiledAt); err != nil {
		return err
	}
	if err := writeLenString(w, a.Header.VersionHash); err != nil {
		return err
	}
	// Deterministic encoding : we use encoding/json for the payload
	// because Go's json.Marshal sorts map keys alphabetically by
	// design — required for content-addressable caching, reproducible
	// builds and signed releases. msgpack v5 was tried first but its
	// SetSortMapKeys flag only sorts a hardcoded subset of map
	// element types (map[string]string|bool|interface{}) and silently
	// random-orders everything else (e.g. map[string]ModuleBlock),
	// which made artifacts byte-unequal across runs.
	payload, err := json.Marshal(a.Definition)
	if err != nil {
		return fmt.Errorf("codegen: encode payload: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(payload))); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

func Decode(r io.Reader) (*Artifact, error) {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("codegen: read magic: %w", err)
	}
	if string(magic) != FileMagic {
		return nil, fmt.Errorf("codegen: bad magic %q (expected %q)", magic, FileMagic)
	}
	var format uint8
	if err := binary.Read(r, binary.BigEndian, &format); err != nil {
		return nil, err
	}
	if format != FormatVersion {
		return nil, fmt.Errorf("codegen: unsupported format version %d (this build expects %d)", format, FormatVersion)
	}
	compilerVer, err := readLenString(r)
	if err != nil {
		return nil, err
	}
	var compiledAt int64
	if err := binary.Read(r, binary.BigEndian, &compiledAt); err != nil {
		return nil, err
	}
	hash, err := readLenString(r)
	if err != nil {
		return nil, err
	}
	var payloadLen uint32
	if err := binary.Read(r, binary.BigEndian, &payloadLen); err != nil {
		return nil, err
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	def := &schema.AppDefinition{}
	if err := json.Unmarshal(payload, def); err != nil {
		return nil, fmt.Errorf("codegen: decode payload: %w", err)
	}
	return &Artifact{
		Header: Header{
			Magic:           [4]byte{magic[0], magic[1], magic[2], magic[3]},
			Format:          format,
			CompilerVersion: compilerVer,
			CompiledAt:      compiledAt,
			VersionHash:     hash,
		},
		Definition: def,
	}, nil
}

func writeLenString(w io.Writer, s string) error {
	if err := binary.Write(w, binary.BigEndian, uint16(len(s))); err != nil {
		return err
	}
	_, err := w.Write([]byte(s))
	return err
}

func readLenString(r io.Reader) (string, error) {
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func EncodeBytes(a *Artifact) ([]byte, error) {
	var buf bytes.Buffer
	if err := Encode(&buf, a); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
