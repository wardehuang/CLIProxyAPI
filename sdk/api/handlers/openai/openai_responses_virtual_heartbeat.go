package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const virtualResponsesStreamHeartbeatInterval = 10 * time.Second

type virtualResponsesStreamResponse struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	CreatedAt  int64  `json:"created_at"`
	Status     string `json:"status"`
	Background bool   `json:"background"`
	Error      any    `json:"error"`
	Output     []any  `json:"output"`
	Model      string `json:"model,omitempty"`
}

type virtualResponsesStreamEvent struct {
	Type           string                         `json:"type"`
	SequenceNumber int64                          `json:"sequence_number"`
	Response       virtualResponsesStreamResponse `json:"response"`
}

type virtualResponsesStreamHeartbeat struct {
	ctx               context.Context
	responseID        string
	model             string
	createdAt         int64
	nextSequence      int64
	dropHandshakeData bool
	pending           []byte
	headers           http.Header
	stopChan          chan struct{}
	doneChan          chan struct{}
	stopOnce          sync.Once
	started           bool
	publish           func([]byte, http.Header) bool
}

var _ coreexecutor.XAIResponsesStreamHeartbeat = (*virtualResponsesStreamHeartbeat)(nil)

func newVirtualResponsesStreamHeartbeat(ctx context.Context, publish func([]byte, http.Header) bool) *virtualResponsesStreamHeartbeat {
	return &virtualResponsesStreamHeartbeat{ctx: ctx, publish: publish}
}

func (h *virtualResponsesStreamHeartbeat) event(eventType string, sequenceNumber int64) []byte {
	payload := virtualResponsesStreamEvent{
		Type:           eventType,
		SequenceNumber: sequenceNumber,
		Response: virtualResponsesStreamResponse{
			ID:         h.responseID,
			Object:     "response",
			CreatedAt:  h.createdAt,
			Status:     "in_progress",
			Background: false,
			Error:      nil,
			Output:     []any{},
			Model:      h.model,
		},
	}
	data, _ := json.Marshal(payload)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data))
}

func (h *virtualResponsesStreamHeartbeat) Start(requestID, model string, headers http.Header) {
	if h.started {
		return
	}
	h.responseID = "resp_" + requestID
	h.model = model
	h.createdAt = time.Now().Unix()
	h.headers = headers
	h.stopChan = make(chan struct{})
	h.doneChan = make(chan struct{})
	h.started = true
	for _, eventType := range []string{"response.created", "response.in_progress"} {
		if !h.publish(h.event(eventType, h.nextSequence), headers) {
			close(h.doneChan)
			return
		}
		h.nextSequence++
	}
	go h.run()
}

func (h *virtualResponsesStreamHeartbeat) run() {
	ticker := time.NewTicker(virtualResponsesStreamHeartbeatInterval)
	defer ticker.Stop()
	defer close(h.doneChan)
	for {
		select {
		case <-h.stopChan:
			return
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			payload := h.event("response.in_progress", h.nextSequence)
			h.nextSequence++
			if !h.publish(payload, h.headers) {
				return
			}
		}
	}
}

func (h *virtualResponsesStreamHeartbeat) StopAndWait() {
	if !h.started {
		return
	}
	h.stopOnce.Do(func() { close(h.stopChan) })
	<-h.doneChan
	h.flushPending()
}

// flushPending emits the frame that was still incomplete when the upstream stream ended, so
// carrying a partial frame across chunks never drops the last event.
func (h *virtualResponsesStreamHeartbeat) flushPending() {
	if len(h.pending) == 0 {
		return
	}
	pending := h.pending
	h.pending = nil
	// A residual frame without data decodes as an empty payload on clients that dispatch their
	// SSE buffer at EOF, which is the very failure this buffering avoids, so drop it.
	if !bytes.Contains(pending, []byte("data:")) {
		return
	}
	frame := h.rewriteFrame(pending)
	if len(frame) == 0 {
		return
	}
	// Left unterminated on purpose: clients flush a trailing partial frame themselves at EOF.
	h.publish(append([]byte(nil), frame...), h.headers)
}

// Rewrite re-frames one upstream chunk. A chunk boundary can fall inside a frame, so the carried
// partial frame is completed from the next chunk before it is rewritten, and the trailing element
// of a chunk is only terminated when that chunk itself ends a frame. Terminating a partial frame
// splits one event into two SSE frames -- an `event:` line without data and a data-only frame --
// and strict clients (the OpenAI SDK) fail to decode the empty payload with
// "Expecting value: line 1 column 1 (char 0)".
func (h *virtualResponsesStreamHeartbeat) Rewrite(payload []byte) []byte {
	if !h.started || len(payload) == 0 {
		return payload
	}
	if len(h.pending) > 0 {
		payload = append(h.pending, payload...)
		h.pending = nil
	}
	frames := bytes.Split(payload, []byte("\n\n"))
	if !bytes.HasSuffix(payload, []byte("\n\n")) {
		// Copy: the caller owns the incoming chunk buffer.
		h.pending = append([]byte(nil), frames[len(frames)-1]...)
		frames = frames[:len(frames)-1]
	}
	var rewritten []byte
	for index, frame := range frames {
		if len(bytes.TrimSpace(frame)) == 0 {
			if index < len(frames)-1 {
				rewritten = append(rewritten, '\n', '\n')
			}
			continue
		}
		frame = h.rewriteFrame(frame)
		if len(frame) == 0 {
			continue
		}
		rewritten = append(rewritten, frame...)
		rewritten = append(rewritten, '\n', '\n')
	}
	return rewritten
}

func (h *virtualResponsesStreamHeartbeat) rewriteFrame(frame []byte) []byte {
	lines := bytes.Split(frame, []byte("\n"))
	eventType := ""
	var dataLines [][]byte
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			eventType = strings.TrimSpace(string(trimmed[len("event:"):]))
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			dataLines = append(dataLines, bytes.TrimSpace(trimmed[len("data:"):]))
		}
	}
	if len(dataLines) == 0 {
		if eventType == "response.created" || eventType == "response.in_progress" {
			h.dropHandshakeData = true
			return nil
		}
		return frame
	}
	if h.dropHandshakeData {
		h.dropHandshakeData = false
		return nil
	}
	data := bytes.Join(dataLines, []byte("\n"))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !json.Valid(data) {
		return frame
	}
	payloadType := gjson.GetBytes(data, "type").String()
	if eventType == "response.created" || eventType == "response.in_progress" || payloadType == "response.created" || payloadType == "response.in_progress" {
		return nil
	}
	if gjson.GetBytes(data, "sequence_number").Exists() {
		data, _ = sjson.SetBytes(data, "sequence_number", h.nextSequence)
		h.nextSequence++
	}
	if gjson.GetBytes(data, "response.id").Exists() {
		data, _ = sjson.SetBytes(data, "response.id", h.responseID)
	}
	if gjson.GetBytes(data, "response.created_at").Exists() {
		data, _ = sjson.SetBytes(data, "response.created_at", h.createdAt)
	}
	var output bytes.Buffer
	dataWritten := false
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if !dataWritten {
				output.WriteString("data: ")
				output.Write(data)
				dataWritten = true
			}
			continue
		}
		output.Write(line)
		output.WriteByte('\n')
	}
	return bytes.TrimSuffix(output.Bytes(), []byte("\n"))
}
