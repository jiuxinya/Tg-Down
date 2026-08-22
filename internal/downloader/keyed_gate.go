package downloader

import (
	"context"
	"sync"
)

// keyedGate serializes work that addresses the same logical resource while
// allowing unrelated keys to proceed concurrently.
type keyedGate[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*gateEntry
}

type gateEntry struct {
	token chan struct{}
	refs  int
}

func newKeyedGate[K comparable]() *keyedGate[K] {
	return &keyedGate[K]{entries: make(map[K]*gateEntry)}
}

func (g *keyedGate[K]) acquire(ctx context.Context, key K) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	g.mu.Lock()
	entry := g.entries[key]
	if entry == nil {
		entry = &gateEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		g.entries[key] = entry
	}
	entry.refs++
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		g.releaseRef(key, entry)
		return nil, ctx.Err()
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			g.releaseRef(key, entry)
			return nil, err
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.token <- struct{}{}
			g.releaseRef(key, entry)
		})
	}, nil
}

func (g *keyedGate[K]) releaseRef(key K, entry *gateEntry) {
	g.mu.Lock()
	entry.refs--
	if entry.refs == 0 && g.entries[key] == entry {
		delete(g.entries, key)
	}
	g.mu.Unlock()
}
