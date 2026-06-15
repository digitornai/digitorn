package processor

import (
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/channels"
)

// Schedule-carried input blobs (a CV) convert to the daemon wire shape and skip
// empty entries — so the same ref rides every fire.
func TestInputAttachments(t *testing.T) {
	out := inputAttachments([]channels.AttachmentRef{
		{Hash: "abc", Mime: "application/pdf", Size: 1200},
		{Hash: "", Mime: "image/png"}, // empty hash → skipped
	})
	if len(out) != 1 || out[0].Hash != "abc" || out[0].Mime != "application/pdf" || out[0].Size != 1200 {
		t.Fatalf("conversion wrong: %+v", out)
	}
	if inputAttachments(nil) != nil {
		t.Fatal("nil refs must yield nil (no inbound clobber)")
	}
}

// The dated stamp is UTC, sortable, and one-per-second granular.
func TestReportDirStamp(t *testing.T) {
	got := reportDirStamp(time.Date(2026, 6, 12, 9, 7, 3, 0, time.FixedZone("x", 2*3600)))
	if got != "2026-06-12_070703" { // 09:07:03 +02:00 → 07:07:03 UTC
		t.Fatalf("stamp = %q, want 2026-06-12_070703", got)
	}
}

// withReportFolder appends a dated, workdir-relative folder instruction to the
// per-fire message without dropping the original message.
func TestWithReportFolder(t *testing.T) {
	out := withReportFolder("Bonjour !", time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(out, "Bonjour !") {
		t.Fatalf("original message lost: %q", out)
	}
	if !strings.Contains(out, "attachments/2026-06-12_090000/") {
		t.Fatalf("dated folder missing: %q", out)
	}
	if !strings.Contains(out, "downloadable") {
		t.Fatalf("download hint missing: %q", out)
	}
}
