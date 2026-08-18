package main

import (
	"sync"
	"time"
)

type keepaliveScheduleState struct {
	mutex           sync.Mutex
	running         bool
	pending         bool
	nextAtUnixMs    int64
	lastStartedMs   int64
	lastCompletedMs int64
	trigger         chan struct{}
}

func newKeepaliveScheduleState() *keepaliveScheduleState {
	return &keepaliveScheduleState{
		trigger: make(chan struct{}, 1),
	}
}

func (state *keepaliveScheduleState) markWaiting(nextAt time.Time) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.running = false
	if state.pending {
		// 等待期间已收到立即保活，保持 pending，不覆盖成未来时间。
		state.nextAtUnixMs = 0
		return
	}
	if nextAt.IsZero() {
		state.nextAtUnixMs = 0
		return
	}
	state.nextAtUnixMs = nextAt.UnixMilli()
}

func (state *keepaliveScheduleState) markStarted(startedAt time.Time) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.running = true
	state.pending = false
	state.nextAtUnixMs = 0
	state.lastStartedMs = startedAt.UnixMilli()
}

func (state *keepaliveScheduleState) markCompleted(completedAt time.Time) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.running = false
	state.lastCompletedMs = completedAt.UnixMilli()
}

func (state *keepaliveScheduleState) requestNow() (accepted bool, running bool) {
	state.mutex.Lock()
	if state.running {
		state.mutex.Unlock()
		return false, true
	}
	state.pending = true
	state.nextAtUnixMs = 0
	state.mutex.Unlock()

	select {
	case state.trigger <- struct{}{}:
		return true, false
	default:
		// 已有待执行的立即保活请求。
		return true, false
	}
}

func (state *keepaliveScheduleState) waitForNext(ctxDone <-chan struct{}, intervalSeconds int) (triggered bool, cancelled bool) {
	if intervalSeconds < 1 {
		intervalSeconds = 1
	}

	// 若等待开始前已有立即请求，直接返回。
	state.mutex.Lock()
	if state.pending {
		state.mutex.Unlock()
		select {
		case <-state.trigger:
			return true, false
		default:
			return true, false
		}
	}
	state.mutex.Unlock()

	nextAt := time.Now().Add(time.Duration(intervalSeconds) * time.Second)
	state.markWaiting(nextAt)

	timer := time.NewTimer(time.Until(nextAt))
	defer timer.Stop()

	select {
	case <-ctxDone:
		return false, true
	case <-state.trigger:
		return true, false
	case <-timer.C:
		return false, false
	}
}

func (state *keepaliveScheduleState) snapshot(intervalSeconds int) map[string]any {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	return map[string]any{
		"running":         state.running,
		"pending":         state.pending,
		"nextAt":          state.nextAtUnixMs,
		"lastStartedAt":   state.lastStartedMs,
		"lastCompletedAt": state.lastCompletedMs,
		"intervalSeconds": intervalSeconds,
	}
}
