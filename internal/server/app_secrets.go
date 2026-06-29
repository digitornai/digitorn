package server

import "os"

// daemonAppSecrets resolves {{secret.X}} for flow nodes and module config at
// runtime. UI-stored secrets take precedence; env vars remain a dev fallback.
type daemonAppSecrets struct {
	store *secretStore
}

func (d daemonAppSecrets) Get(appID, key string) (string, bool) {
	if d.store != nil {
		if v, ok := d.store.getAny(appID, key); ok && v != "" && v != secretSentinel {
			return v, true
		}
	}
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, true
	}
	return "", false
}
