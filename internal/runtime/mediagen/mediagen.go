// Package mediagen calls the gateway's dedicated image/video generation
// endpoints — these are NOT exposed through /v1/chat/completions, so an
// image/video agent does a single authenticated POST here instead of a chat
// turn. Everything still flows through the gateway (same JWT/sk auth as chat).
//
//	image : POST {base}/v1/images/generations  → {data:[{url|b64_json}]}
//	video : POST {base}/v1/videos/generations  → {video:{url, content_type,…}}
//
// Generated images are downloaded to bytes here so they can be persisted in the
// content-addressed blob store (the gateway/CDN URLs are short-lived). Videos
// stay as URLs (downloading them would be heavy) — the URL is rendered directly.
package mediagen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/llm"
)

// Client POSTs to a gateway's OpenAI-style media endpoints.
type Client struct {
	// Base is the gateway root (may or may not include a trailing /v1 — it's
	// normalised per request).
	Base string
	HTTP *http.Client
}

// New builds a client for the given gateway base URL.
func New(base string) *Client {
	return &Client{
		Base: strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1"),
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) post(ctx context.Context, path string, bearer string, body any, out any) error {
	if c == nil || c.Base == "" {
		return fmt.Errorf("mediagen: no gateway base url configured")
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("mediagen: %s → HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type imageResponse struct {
	Data []struct {
		URL     string `json:"url"`
		B64JSON string `json:"b64_json"`
	} `json:"data"`
}

// GenerateImage requests image generation and returns image parts with inline
// bytes (decoded from b64_json or downloaded from the returned URL).
func (c *Client) GenerateImage(ctx context.Context, model, prompt, bearer string) ([]llm.ContentPart, error) {
	var out imageResponse
	if err := c.post(ctx, "/v1/images/generations", bearer, map[string]any{
		"model":  model,
		"prompt": prompt,
		"n":      1,
	}, &out); err != nil {
		return nil, err
	}
	var parts []llm.ContentPart
	for _, d := range out.Data {
		if d.B64JSON != "" {
			data, err := base64.StdEncoding.DecodeString(d.B64JSON)
			if err != nil || len(data) == 0 {
				continue
			}
			parts = append(parts, llm.ContentPart{Type: llm.ContentTypeImage, Mime: "image/png", Data: data})
			continue
		}
		if d.URL != "" {
			data, mime, err := c.download(ctx, d.URL)
			if err != nil || len(data) == 0 {
				// Keep the URL as a fallback so something still renders.
				parts = append(parts, llm.ContentPart{Type: llm.ContentTypeImage, URL: d.URL})
				continue
			}
			parts = append(parts, llm.ContentPart{Type: llm.ContentTypeImage, Mime: mime, Data: data})
		}
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("mediagen: image response carried no usable data")
	}
	return parts, nil
}

type videoResponse struct {
	Video struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
	} `json:"video"`
}

// GenerateVideo requests video generation and returns a single video part
// pointing at the gateway-hosted URL.
func (c *Client) GenerateVideo(ctx context.Context, model, prompt, bearer string) ([]llm.ContentPart, error) {
	var out videoResponse
	if err := c.post(ctx, "/v1/videos/generations", bearer, map[string]any{
		"model":  model,
		"prompt": prompt,
	}, &out); err != nil {
		return nil, err
	}
	if out.Video.URL == "" {
		return nil, fmt.Errorf("mediagen: video response carried no url")
	}
	mime := out.Video.ContentType
	if mime == "" {
		mime = "video/mp4"
	}
	return []llm.ContentPart{{Type: llm.ContentTypeVideo, URL: out.Video.URL, Mime: mime}}, nil
}

// download fetches a generated-image URL into bytes + mime so it can be stored
// permanently in the blob store (the CDN URLs expire).
func (c *Client) download(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/png"
	}
	return data, mime, nil
}
