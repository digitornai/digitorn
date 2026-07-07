package schema

// Requirement is a system binary the app needs at runtime but does not ship —
// the daemon provisions it (download + verify + extract) once, out-of-band, and
// puts it on the agent's PATH. Declared at the manifest top level:
//
//	requirements:
//	  - id: tectonic
//	    bin: tectonic
//	    version: "0.15.0"
//	    check: "tectonic --version"
//	    platforms:
//	      linux/amd64:  { url: "…", sha256: "…", format: tar.gz, path: tectonic }
//	      darwin/arm64: { url: "…", sha256: "…", format: tar.gz, path: tectonic }
//
// Provisioning is asynchronous and consent-gated : the daemon never downloads
// without the user opting in, and never blocks a request on a download.
type Requirement struct {
	// ID is the logical name (stable across versions), e.g. "tectonic".
	ID string `yaml:"id" json:"id"`
	// Bin is the executable name that must resolve on PATH once provisioned.
	// Defaults to ID when empty.
	Bin string `yaml:"bin,omitempty" json:"bin,omitempty"`
	// Version pins the artifact; part of the on-disk cache key so a bump
	// re-provisions.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Label is a human-facing name for the consent dialog; defaults to ID.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	// Description is shown to the user in the consent dialog (why it's needed).
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Check is an optional smoke-test command run after install to confirm the
	// binary works (e.g. "tectonic --version"). Empty skips the check.
	Check string `yaml:"check,omitempty" json:"check,omitempty"`
	// Platforms maps "<os>/<arch>" (Go's runtime.GOOS/GOARCH) to the artifact to
	// fetch on that platform. A host with no matching entry is unsupported.
	Platforms map[string]PlatformArtifact `yaml:"platforms" json:"platforms"`
}

// PlatformArtifact describes the downloadable for one os/arch.
type PlatformArtifact struct {
	// URL to download.
	URL string `yaml:"url" json:"url"`
	// SHA256 is the lowercase hex digest of the downloaded bytes — MANDATORY;
	// the daemon refuses to install an artifact whose digest does not match.
	SHA256 string `yaml:"sha256" json:"sha256"`
	// Format of the download: "tar.gz" | "tgz" | "zip" | "binary" (raw
	// executable) | "gz" (single gzipped binary). Defaults to "binary".
	Format string `yaml:"format,omitempty" json:"format,omitempty"`
	// Path, for archives, is the path WITHIN the archive to the executable to
	// expose. Ignored for "binary". Defaults to the requirement's Bin.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// SizeBytes is the approximate download size, surfaced in the consent dialog.
	SizeBytes int64 `yaml:"size_bytes,omitempty" json:"size_bytes,omitempty"`
}

// EffectiveBin returns the executable name (Bin or ID).
func (r Requirement) EffectiveBin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return r.ID
}

// EffectiveLabel returns the human name for the consent UI.
func (r Requirement) EffectiveLabel() string {
	if r.Label != "" {
		return r.Label
	}
	return r.ID
}
