package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The model an upstream response serves is declared in the response payload,
// not in a response header: Codex Responses events carry it as response.model
// (streaming) or model (non-streaming body). Header-only observation cannot
// see it at all, so the observer below reads payloads in memory and keeps
// nothing but the model name. Bodies are never persisted.
const (
	upstreamModelMaxLength = 200
	// A frame split across chunk boundaries is buffered until the next chunk
	// completes it. The cap bounds that buffer: a single SSE frame larger than
	// this is abandoned rather than grown without limit.
	maxStreamResidual = 64 << 10
)

// modelObserver tracks the model an upstream response declares for one
// attempt. A terminal declaration wins over earlier ones; otherwise the first
// declaration is kept. Two declarations that disagree raise a conflict, which
// is reported rather than resolved by guessing.
type modelObserver struct {
	first    string
	terminal string
	conflict bool
	residual []byte
}

func (o *modelObserver) observe(model string, terminal bool) {
	model = normalizeObservedModel(model)
	if model == "" {
		return
	}
	if current := o.model(); current != "" && !strings.EqualFold(current, model) {
		o.conflict = true
	}
	if terminal {
		o.terminal = model
		return
	}
	if o.first == "" {
		o.first = model
	}
}

func (o *modelObserver) model() string {
	if o == nil {
		return ""
	}
	if o.terminal != "" {
		return o.terminal
	}
	return o.first
}

func (o *modelObserver) conflicted() bool { return o != nil && o.conflict }

func normalizeObservedModel(model string) string {
	model = strings.TrimSpace(model)
	if runes := []rune(model); len(runes) > upstreamModelMaxLength {
		return string(runes[:upstreamModelMaxLength])
	}
	return model
}

// observePayload reads one JSON payload. eventType decides whether the
// declaration is terminal; an empty event type means the payload is a whole
// response body rather than a stream frame.
func (o *modelObserver) observePayload(payload []byte, eventType string) {
	if o == nil || len(bytes.TrimSpace(payload)) == 0 {
		return
	}
	var declared struct {
		Type     string `json:"type"`
		Model    string `json:"model"`
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &declared); err != nil {
		return
	}
	model := declared.Response.Model
	if model == "" {
		model = declared.Model
	}
	if model == "" {
		model = declared.Message.Model
	}
	if declared.Type != "" {
		eventType = declared.Type
	}
	o.observe(model, eventType == "" || isTerminalModelEvent(eventType))
}

// CPA callbacks may contain a complete JSON event or data line without SSE
// separators. Only consume independently valid JSON at that boundary; partial
// data continues through the byte-stream parser.
func (o *modelObserver) observeCallback(chunk []byte) {
	if o == nil {
		return
	}
	if len(bytes.TrimSpace(o.residual)) == 0 {
		payload := bytes.TrimSpace(chunk)
		if bytes.HasPrefix(payload, []byte("data:")) {
			payload = bytes.TrimSpace(payload[5:])
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			o.residual = nil
			return
		}
		if json.Valid(payload) {
			o.residual = nil
			o.observePayload(payload, "chunk")
			return
		}
	}
	o.observeStream(chunk)
}

// observeBody reads a complete response body, whether it is an SSE stream that
// the host handed over in one piece or a single JSON document.
func (o *modelObserver) observeBody(body []byte) {
	if o == nil {
		return
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		o.observePayload(trimmed, "")
		return
	}
	o.observeStream(body)
	o.flushStream()
}

// observeStream feeds one streamed chunk. Chunk boundaries do not align with
// SSE frame boundaries, so an incomplete trailing frame is buffered for the
// next chunk instead of being parsed and discarded.
func (o *modelObserver) observeStream(chunk []byte) {
	if o == nil || len(chunk) == 0 {
		return
	}
	data := chunk
	if len(o.residual) > 0 {
		data = append(o.residual, chunk...)
		o.residual = nil
	}
	// Normalize after combining residual data so split CRLF pairs are handled.
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	for {
		index := bytes.Index(data, []byte("\n\n"))
		if index < 0 {
			break
		}
		o.observeFrame(data[:index])
		data = data[index+2:]
	}
	if len(data) > maxStreamResidual {
		return
	}
	o.residual = append([]byte(nil), data...)
}

// flushStream parses whatever trailing frame remains once no further chunk can
// arrive. A stream whose final frame is not terminated by a blank line still
// gets read.
func (o *modelObserver) flushStream() {
	if o == nil || len(o.residual) == 0 {
		return
	}
	residual := o.residual
	o.residual = nil
	o.observeFrame(residual)
}

func (o *modelObserver) observeFrame(frame []byte) {
	eventType := ""
	var payload []byte
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := bytes.TrimRight(rawLine, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventType = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			// SSE joins consecutive data lines of one frame with newlines.
			if len(payload) > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, bytes.TrimSpace(line[len("data:"):])...)
		}
	}
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	// A frame with data but no event line is a chat-completions style chunk;
	// treat it as non-terminal unless its payload says otherwise.
	if eventType == "" {
		eventType = "chunk"
	}
	o.observePayload(payload, eventType)
}

func isTerminalModelEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

// modelMismatch reports whether the upstream served a model other than the one
// sent. A response that never declared a model returns nil: unknown is not the
// same answer as match, and reporting it as a match would hide exactly the case
// this check exists for.
func modelMismatch(sentModel, upstreamModel string) *bool {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return nil
	}
	sentModel = strings.TrimSpace(sentModel)
	mismatch := sentModel == "" || !strings.EqualFold(sentModel, upstreamModel)
	return &mismatch
}

// sentModel is the model that actually went upstream. The host reports the
// mapped model in Model and the caller's original ask in RequestedModel; only
// the former describes what the upstream was asked for.
func sentModel(model, requestedModel string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(requestedModel)
}
