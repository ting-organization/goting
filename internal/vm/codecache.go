package vm

import (
	"container/list"
	"sync"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/wasm"
)

// CodeCache keeps validated, compiled modules by CodeHash, including the
// verdict for invalid code. It is a node optimization only: charges never
// depend on it (rules §23.4). It is safe for concurrent use.
type CodeCache struct {
	mu      sync.Mutex
	entries map[hashing.Hash]*list.Element
	order   *list.List // front = most recently used
	size    int64
	max     int64
}

type cacheEntry struct {
	hash hashing.Hash
	mod  *wasm.Module
	err  error
	cost int64
}

// NewCodeCache returns a cache bounded to roughly maxBytes of decoded
// modules, each counted by its Weight.
func NewCodeCache(maxBytes int64) *CodeCache {
	return &CodeCache{entries: make(map[hashing.Hash]*list.Element), order: list.New(), max: maxBytes}
}

// Get returns a cached valid module, or nil.
func (c *CodeCache) Get(h hashing.Hash) *wasm.Module {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[h]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*cacheEntry).mod
	}
	return nil
}

// Validate returns the module for code whose CodeHash is h, decoding and
// validating it unless the verdict is cached. It returns nil and the
// rejection for invalid code.
func (c *CodeCache) Validate(h hashing.Hash, code []byte) (*wasm.Module, error) {
	c.mu.Lock()
	if e, ok := c.entries[h]; ok {
		c.order.MoveToFront(e)
		ce := e.Value.(*cacheEntry)
		c.mu.Unlock()
		return ce.mod, ce.err
	}
	c.mu.Unlock()
	mod, err := wasm.Decode(code)
	cost := int64(128)
	if err == nil {
		cost = mod.Weight()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[h]; ok {
		ce := e.Value.(*cacheEntry)
		return ce.mod, ce.err
	}
	if cost <= c.max {
		e := c.order.PushFront(&cacheEntry{hash: h, mod: mod, err: err, cost: cost})
		c.entries[h] = e
		c.size += cost
		for c.size > c.max {
			last := c.order.Back()
			ce := last.Value.(*cacheEntry)
			c.order.Remove(last)
			delete(c.entries, ce.hash)
			c.size -= ce.cost
		}
	}
	return mod, err
}
