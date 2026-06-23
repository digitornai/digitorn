package agent

import "sync"

type KV struct {
	mu    sync.RWMutex
	roots map[string]map[string]string
}

func NewKV() *KV {
	return &KV{roots: map[string]map[string]string{}}
}

func (k *KV) Set(root, key, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.roots[root] == nil {
		k.roots[root] = map[string]string{}
	}
	k.roots[root][key] = value
}

func (k *KV) Get(root, key string) (string, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if m, ok := k.roots[root]; ok {
		v, ok := m[key]
		return v, ok
	}
	return "", false
}

func (k *KV) Delete(root, key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if m := k.roots[root]; m != nil {
		delete(m, key)
	}
}

func (k *KV) All(root string) map[string]string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	m := k.roots[root]
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for key, val := range m {
		out[key] = val
	}
	return out
}

func (k *KV) Clear(root string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.roots, root)
}
