package server

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/llm"
)

// transcribeMaxBytes bounds an uploaded voice-note clip. Composer
// recordings are seconds long; 25 MB matches the gateway's soft limit
// so oversized clips fail here instead of after a full upload hop.
const transcribeMaxBytes = 25 << 20

// transcribeAudio implements POST /api/transcribe — the web composer's
// voice input (voice-input.ts "whisper" mode). Multipart fields:
//
//	audio     (required) recorded blob — webm/opus from MediaRecorder
//	language  (optional) BCP-47 hint from the browser ("fr-FR")
//	app_id    (optional) attribution forwarded for per-app accounting
//
// The audio rides the same path as voice calls: llm worker → bifrost →
// gateway, authenticated with the CALLER's JWT so quota/cost land on
// the user, not on a daemon-wide credential. Response: {"text": "…"}.
func (d *Daemon) transcribeAudio(w http.ResponseWriter, r *http.Request) {
	if d.llmClient == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "transcription requires the LLM worker")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, transcribeMaxBytes)
	if err := r.ParseMultipartForm(transcribeMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_multipart", err.Error())
		return
	}
	file, header, err := r.FormFile("audio")
	if err != nil {
		// OpenAI-style field name as a courtesy for CLI/script callers.
		file, header, err = r.FormFile("file")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_file", "form field 'audio' required")
		return
	}
	defer file.Close()
	audio, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_error", err.Error())
		return
	}
	if len(audio) == 0 {
		writeError(w, http.StatusBadRequest, "empty_audio", "audio payload is empty")
		return
	}

	model := d.cfg.Voice.STTModel
	if model == "" {
		model = "whisper-1"
	}
	// Whisper expects ISO-639-1; browsers send BCP-47 ("fr-FR" → "fr").
	lang := strings.TrimSpace(r.FormValue("language"))
	if i := strings.IndexByte(lang, '-'); i > 0 {
		lang = lang[:i]
	}

	text, err := d.llmClient.TranscribeText(r.Context(), &llm.TranscribeRequest{
		Model:    model,
		Audio:    audio,
		Format:   uploadAudioFormat(header.Filename, header.Header.Get("Content-Type")),
		Language: lang,
		UserJWT:  extractBearer(r),
		AppID:    strings.TrimSpace(r.FormValue("app_id")),
		UserID:   userIDOf(r.Context()),
		Timeout:  60 * time.Second,
	})
	if err != nil {
		// Decode the worker's gRPC status so a quota block reaches the
		// browser as 429 code=quota_exceeded (→ "upgrade your plan"),
		// not a generic 502. See llm_error.go.
		d.writeLLMError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": strings.TrimSpace(text)})
}

// uploadAudioFormat maps the uploaded blob's filename/MIME onto the
// container names llm's audioExt understands. MediaRecorder produces
// audio/webm (Chrome/Firefox) or audio/mp4 (Safari).
func uploadAudioFormat(filename, contentType string) string {
	name := strings.ToLower(filename)
	for _, ext := range []string{"webm", "mp3", "ogg", "flac", "opus", "m4a", "wav"} {
		if strings.HasSuffix(name, "."+ext) {
			return ext
		}
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "webm"):
		return "webm"
	case strings.Contains(ct, "mp4"), strings.Contains(ct, "m4a"), strings.Contains(ct, "aac"):
		return "m4a"
	case strings.Contains(ct, "mpeg"), strings.Contains(ct, "mp3"):
		return "mp3"
	case strings.Contains(ct, "ogg"), strings.Contains(ct, "opus"):
		return "ogg"
	}
	return "wav"
}
