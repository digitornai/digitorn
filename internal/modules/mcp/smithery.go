package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func resolveSmithery(id string, sc schema.MCPServerConfig) (connectSpec, error) {
	key := sc.SmitheryKey
	if key == "" {
		return connectSpec{}, fmt.Errorf("mcp: server %q uses via:smithery but no smithery_key", id)
	}
	entry, hasEntry := catalogLookup(id)

	slug := sc.SmitherySlug
	if slug == "" && hasEntry {
		slug = entry.SmitherySlug
	}
	if slug == "" {
		slug = smitherySlugs[id]
	}
	if slug == "" {
		slug = id
	}

	var endpoint string
	if sc.SmitheryNS != "" {
		endpoint = fmt.Sprintf("%s/%s/%s/mcp", smitheryConnectBase, sc.SmitheryNS, id)
	} else {
		endpoint = fmt.Sprintf("%s/%s/mcp", smitheryProxyBase, slug)
	}

	cfg := map[string]string{}
	for k, v := range sc.Extra {
		if standardKeys[k] {
			continue
		}
		s := fmt.Sprintf("%v", v)
		if hasEntry {
			if envVar, ok := entry.EnvMapping[k]; ok {
				if envVar != argAppend {
					cfg[envVar] = s
				} else {
					cfg[k] = s
				}
				continue
			}
		}
		cfg[k] = s
	}
	if len(cfg) > 0 {
		b, err := json.Marshal(cfg)
		if err != nil {
			return connectSpec{}, err
		}
		endpoint = endpoint + "?config=" + url.QueryEscape(string(b))
	}

	timeout := defaultTimeout
	if sc.Timeout > 0 {
		timeout = time.Duration(sc.Timeout * float64(time.Second))
	}
	return connectSpec{
		Transport: "streamable_http",
		URL:       endpoint,
		Headers:   map[string]string{"Authorization": "Bearer " + key},
		Timeout:   timeout,
	}, nil
}
