package sessionstore

import (
	"fmt"
	"sync"
	"sync/atomic"
)

type SeqAllocator struct {
	next atomic.Uint64
}

func NewSeqAllocator(highestIssued uint64) *SeqAllocator {
	a := &SeqAllocator{}
	a.next.Store(highestIssued)
	return a
}

func (a *SeqAllocator) Next() uint64 {
	return a.next.Add(1)
}

func (a *SeqAllocator) Current() uint64 {
	return a.next.Load()
}

func (a *SeqAllocator) Bump(toAtLeast uint64) {
	for {
		cur := a.next.Load()
		if cur >= toAtLeast {
			return
		}
		if a.next.CompareAndSwap(cur, toAtLeast) {
			return
		}
	}
}

func RecoverSeq(p Paths, sid string) (uint64, error) {
	if sid == "" {
		return 0, fmt.Errorf("recover seq: empty session id")
	}
	var maxSeq uint64

	if meta, err := ReadMeta(p.MetaFile(sid)); err == nil && meta != nil {
		if meta.LastSeq > maxSeq {
			maxSeq = meta.LastSeq
		}
	}

	if snap, _, err := ReadSnapshot(p.SessionDir(sid)); err == nil && snap != nil {
		if snap.LastSeq > maxSeq {
			maxSeq = snap.LastSeq
		}
		if snap.CutoffSeq > maxSeq {
			maxSeq = snap.CutoffSeq
		}
	}

	jres, err := ReadJSONL(p.EventsFile(sid), JSONLBestEffort, "")
	if err != nil {
		return maxSeq, nil
	}
	if jres != nil && jres.LastGoodSeq > maxSeq {
		maxSeq = jres.LastGoodSeq
	}
	return maxSeq, nil
}

type SeqRegistry struct {
	paths Paths
	mu    sync.Mutex
	all   map[string]*SeqAllocator
}

func NewSeqRegistry(p Paths) *SeqRegistry {
	return &SeqRegistry{paths: p, all: map[string]*SeqAllocator{}}
}

func (r *SeqRegistry) For(sid string) (*SeqAllocator, error) {
	r.mu.Lock()
	if a, ok := r.all[sid]; ok {
		r.mu.Unlock()
		return a, nil
	}
	r.mu.Unlock()

	start, err := RecoverSeq(r.paths, sid)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.all[sid]; ok {
		return a, nil
	}
	a := NewSeqAllocator(start)
	r.all[sid] = a
	return a, nil
}

func (r *SeqRegistry) Drop(sid string) {
	r.mu.Lock()
	delete(r.all, sid)
	r.mu.Unlock()
}

func (r *SeqRegistry) Snapshot() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]uint64, len(r.all))
	for sid, a := range r.all {
		out[sid] = a.Current()
	}
	return out
}
