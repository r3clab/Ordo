package image

import (
	"context"
	"sync"
)

type call[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// A waiter that gives up leaves the call running for later callers.
type flight[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*call[V]
	wg    sync.WaitGroup
}

func (f *flight[K, V]) do(ctx context.Context, key K, fn func() (V, error)) (V, error) {
	f.mu.Lock()
	c, ok := f.calls[key]
	if !ok {
		if f.calls == nil {
			f.calls = map[K]*call[V]{}
		}
		c = &call[V]{done: make(chan struct{})}
		f.calls[key] = c
		f.wg.Go(func() {
			c.val, c.err = fn()
			f.mu.Lock()
			delete(f.calls, key)
			f.mu.Unlock()
			close(c.done)
		})
	}
	f.mu.Unlock()
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

func (f *flight[K, V]) wait() { f.wg.Wait() }
