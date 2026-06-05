package sessionstore

import (
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"strings"
)

const (
	eventsFilename      = "events.jsonl"
	metaFilename        = "meta.json"
	snapshotFilename    = "snapshot.json"
	snapshotBinFilename = "snapshot.bin"
	quarantineFilename  = "quarantine.jsonl"
	blobsDirname        = "blobs"
	tmpSnapshotPrefix   = ".snapshot.tmp."
	tmpEventsPrefix     = ".events.tmp."
	tmpMetaPrefix       = ".meta.tmp."
)

type Paths struct {
	Root string
}

func NewPaths(root string) Paths { return Paths{Root: root} }

func (p Paths) SessionDir(sid string) string {
	bucket := shardBucket(sid)
	return filepath.Join(p.Root, bucket, encodeSessionDir(sid))
}

// encodeSessionDir maps a session ID to a filesystem-safe directory-name
// component. Normal session IDs (UUIDs, alphanumerics, '-' '_' '.' '#' '~')
// pass through UNCHANGED, so existing on-disk sessions keep their directory —
// no migration. IDs containing characters that are illegal in a path component
// on some OS (most importantly ':' on Windows, used by sub-agent session IDs
// like "<root>::agent::<id>#<hash>") are percent-escaped (%XX, uppercase hex).
//
// The encoding is reversible (decodeSessionDir) and injective, so distinct IDs
// never collide on one directory. The shard bucket is still computed from the
// RAW id, so routing is unchanged.
func encodeSessionDir(sid string) string {
	if isSafeDirName(sid) {
		return sid
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(sid) + 8)
	for i := 0; i < len(sid); i++ {
		c := sid[i]
		if safeDirByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// decodeSessionDir is the inverse of encodeSessionDir. A name with no '%'
// passes through unchanged (the common case + every legacy directory).
func decodeSessionDir(name string) string {
	if !strings.Contains(name, "%") {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		if name[i] == '%' && i+2 < len(name) {
			hi := unhexDigit(name[i+1])
			lo := unhexDigit(name[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

// DecodeSessionDir is the exported inverse used by callers that recover a
// session ID from an on-disk directory name (e.g. the session-list scan).
func DecodeSessionDir(name string) string { return decodeSessionDir(name) }

func isSafeDirName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !safeDirByte(s[i]) {
			return false
		}
	}
	return true
}

// safeDirByte reports whether c is safe verbatim in a path component on every
// supported OS. Deliberately conservative : letters, digits, and a small set
// of punctuation known-safe on Windows/macOS/Linux. '%' is NOT safe (it's the
// escape marker, so it must itself be escaped to keep decoding unambiguous).
func safeDirByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '-' || c == '_' || c == '.' || c == '#' || c == '~':
		return true
	}
	return false
}

func unhexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

func (p Paths) EventsFile(sid string) string { return filepath.Join(p.SessionDir(sid), eventsFilename) }
func (p Paths) MetaFile(sid string) string   { return filepath.Join(p.SessionDir(sid), metaFilename) }
func (p Paths) SnapshotFile(sid string) string {
	return filepath.Join(p.SessionDir(sid), snapshotFilename)
}
func (p Paths) SnapshotBinFile(sid string) string {
	return filepath.Join(p.SessionDir(sid), snapshotBinFilename)
}
func (p Paths) QuarantineFile(sid string) string {
	return filepath.Join(p.SessionDir(sid), quarantineFilename)
}
func (p Paths) BlobsDir(sid string) string { return filepath.Join(p.SessionDir(sid), blobsDirname) }

func shardBucket(sid string) string {
	if sid == "" {
		return "00"
	}
	h := sha256.Sum256([]byte(sid))
	idx := binary.BigEndian.Uint16(h[:2]) % 256
	var b strings.Builder
	const hex = "0123456789abcdef"
	b.WriteByte(hex[idx>>4])
	b.WriteByte(hex[idx&0x0f])
	return b.String()
}

func ShardOf(sid string, numShards int) int {
	if numShards <= 0 {
		return 0
	}
	h := sha256.Sum256([]byte(sid))
	return int(binary.BigEndian.Uint32(h[:4])) % numShards
}
