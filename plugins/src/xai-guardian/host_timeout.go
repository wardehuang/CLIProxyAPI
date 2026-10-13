package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// defaultHostCallTimeout bounds a single plugin -> host callback so a stalled
// callback cannot freeze a long-running batch job forever.
const defaultHostCallTimeout = 15 * time.Second

// hostCaller abstracts the synchronous host callback so call sites can pick the
// plain call or a timeout-guarded variant.
type hostCaller func(method string, payload any) (json.RawMessage, error)

// callHostWithTimeout runs the host callback in a goroutine and gives up waiting
// once timeout or ctx elapses.
//
// The underlying callback is a synchronous cgo call that cannot be interrupted,
// so a timed-out call may still finish later inside its goroutine; the caller
// stops waiting and reports a timeout instead of blocking indefinitely. Callers
// must only use this where giving up is safe (per-item batch work), not where a
// late completion would corrupt state.
func callHostWithTimeout(ctx context.Context, method string, payload any, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = defaultHostCallTimeout
	}
	type outcome struct {
		raw json.RawMessage
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		raw, err := callHost(method, payload)
		done <- outcome{raw: raw, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}
	select {
	case result := <-done:
		return result.raw, result.err
	case <-timer.C:
		return nil, fmt.Errorf("host callback %s timed out after %s", method, timeout)
	case <-ctxDone:
		return nil, fmt.Errorf("host callback %s cancelled: %w", method, ctx.Err())
	}
}
