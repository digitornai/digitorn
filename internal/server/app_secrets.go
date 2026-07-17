package server

import "os"

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
