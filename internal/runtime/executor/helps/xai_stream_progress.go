package helps

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// XAIStreamProgressWatchdog cancels only the current upstream attempt when it stalls.
// It is enabled exclusively by the realtime-guard plugin's request metadata.
type XAIStreamProgressWatchdog struct {
	mutex    sync.Mutex
	ctx      context.Context
	cancel   context.CancelCauseFunc
	timeout  time.Duration
	deadline time.Time
	timer    *time.Timer
	stopped  bool
}

func NewXAIStreamProgressWatchdog(ctx context.Context, cancel context.CancelCauseFunc, timeout time.Duration) *XAIStreamProgressWatchdog {
	watchdog := &XAIStreamProgressWatchdog{ctx: ctx, cancel: cancel, timeout: timeout, stopped: timeout <= 0}
	if timeout > 0 {
		watchdog.mutex.Lock()
		watchdog.deadline = time.Now().Add(timeout)
		watchdog.timer = time.AfterFunc(timeout, watchdog.expire)
		watchdog.mutex.Unlock()
	}
	return watchdog
}

func (watchdog *XAIStreamProgressWatchdog) expire() {
	watchdog.mutex.Lock()
	defer watchdog.mutex.Unlock()
	if watchdog.stopped || watchdog.ctx.Err() != nil {
		return
	}
	// A previously queued callback must honor progress that reset the deadline.
	if remaining := time.Until(watchdog.deadline); remaining > 0 {
		watchdog.timer.Reset(remaining)
		return
	}
	watchdog.stopped = true
	watchdog.cancel(fmt.Errorf("%w: no output, reasoning or tool-call progress for %s", cliproxyexecutor.ErrStreamProgressTimeout, watchdog.timeout))
}

// Observe accepts upstream JSON data events, never SSE comments or downstream heartbeats.
func (watchdog *XAIStreamProgressWatchdog) Observe(eventData []byte) {
	if !xaiStreamEventHasProgress(eventData) {
		return
	}
	watchdog.mutex.Lock()
	defer watchdog.mutex.Unlock()
	if watchdog.stopped || watchdog.ctx.Err() != nil {
		return
	}
	watchdog.deadline = time.Now().Add(watchdog.timeout)
	watchdog.timer.Reset(watchdog.timeout)
}

func (watchdog *XAIStreamProgressWatchdog) Stop() {
	watchdog.mutex.Lock()
	defer watchdog.mutex.Unlock()
	if !watchdog.stopped {
		watchdog.stopped = true
		watchdog.timer.Stop()
	}
}

func xaiStreamEventHasProgress(eventData []byte) bool {
	event := gjson.ParseBytes(eventData)
	eventType := event.Get("type").String()
	switch eventType {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.code_interpreter_call_code.delta":
		return event.Get("delta").Type == gjson.String && event.Get("delta").String() != ""
	case "response.output_text.done", "response.reasoning_summary_text.done", "response.reasoning_text.done":
		return event.Get("text").String() != ""
	case "response.function_call_arguments.done":
		return event.Get("arguments").String() != ""
	case "response.custom_tool_call_input.done":
		return event.Get("input").String() != ""
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return event.Get("part.text").String() != ""
	case "response.output_item.added", "response.output_item.done":
		item := event.Get("item")
		if strings.HasSuffix(item.Get("type").String(), "_call") || item.Get("encrypted_content").String() != "" {
			return true
		}
		for _, field := range []string{"content", "summary"} {
			for _, part := range item.Get(field).Array() {
				if part.Get("text").String() != "" {
					return true
				}
			}
		}
	case "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed",
		"response.code_interpreter_call.in_progress", "response.code_interpreter_call.interpreting", "response.code_interpreter_call.completed":
		return true
	}
	if strings.EqualFold(event.Get("delta_type").String(), "thinking_delta") && event.Get("delta").Type == gjson.String {
		return event.Get("delta").String() != ""
	}
	if strings.EqualFold(event.Get("delta.type").String(), "thinking_delta") {
		return event.Get("delta.thinking").String() != "" || event.Get("delta.text").String() != ""
	}
	return false
}
