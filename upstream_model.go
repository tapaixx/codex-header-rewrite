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
	// The reasoning effort the response reports, tracked the same way as the
	// model. It is recorded beside the model and never compared: an effort is
	// a setting, not a claim about which model answered, so a difference here
	// is not a mismatch.
	effortFirst    string
	effortTerminal string
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

func (o *modelObserver) observeEffort(effort string, terminal bool) {
	effort = strings.TrimSpace(effort)
	if effort == "" || len([]rune(effort)) > 32 {
		return
	}
	if terminal {
		o.effortTerminal = effort
		return
	}
	if o.effortFirst == "" {
		o.effortFirst = effort
	}
}

func (o *modelObserver) effort() string {
	if o == nil {
		return ""
	}
	if o.effortTerminal != "" {
		return o.effortTerminal
	}
	return o.effortFirst
}

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
// It reports whether the payload parsed, which is what tells a caller the
// bytes it held were a complete frame rather than a truncated one.
func (o *modelObserver) observePayload(payload []byte, eventType string) bool {
	trimmed := bytes.TrimSpace(payload)
	if o == nil || len(trimmed) == 0 {
		return false
	}
	// A whole stream can arrive as one JSON array of events rather than as an
	// object. Unmarshalling that into the struct below fails, which read as
	// "declared nothing" and lost every event in it.
	if trimmed[0] == '[' {
		var elements []json.RawMessage
		if err := json.Unmarshal(trimmed, &elements); err != nil {
			return false
		}
		parsed := false
		for _, element := range elements {
			if o.observePayload(element, eventType) {
				parsed = true
			}
		}
		return parsed
	}
	var declared struct {
		Type     string `json:"type"`
		Model    string `json:"model"`
		Response struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		} `json:"response"`
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		// Some hosts nest the event under a data key instead of sending it at
		// the top level.
		Data struct {
			Type     string `json:"type"`
			Model    string `json:"model"`
			Response struct {
				Model string `json:"model"`
			} `json:"response"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &declared); err != nil {
		return false
	}
	model := firstNonEmpty(
		declared.Response.Model, declared.Model, declared.Message.Model,
		declared.Data.Response.Model, declared.Data.Model, declared.Data.Message.Model,
	)
	if declared.Type != "" {
		eventType = declared.Type
	} else if declared.Data.Type != "" {
		eventType = declared.Data.Type
	}
	terminal := eventType == "" || isTerminalModelEvent(eventType)
	o.observe(model, terminal)
	o.observeEffort(firstNonEmpty(declared.Response.Reasoning.Effort, declared.Reasoning.Effort), terminal)
	return true
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
	if len(bytes.TrimSpace(data)) == 0 {
		return
	}
	// A host that hands over one event per callback strips the blank line, so
	// there is no separator left to find and waiting for one would buffer the
	// entire stream and read none of it. Read the tail: what is complete is
	// consumed, what is truncated stays for the next chunk.
	if consumed := o.observeTail(data); consumed > 0 {
		data = data[consumed:]
	}
	if len(bytes.TrimSpace(data)) == 0 || len(data) > maxStreamResidual {
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

// parseSSEFrame splits one frame into its event type, its joined data payload
// and the individual data lines it was joined from.
func parseSSEFrame(frame []byte) (string, []byte, [][]byte) {
	eventType := ""
	var payload []byte
	var lines [][]byte
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := bytes.TrimRight(rawLine, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventType = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			value := bytes.TrimSpace(line[len("data:"):])
			lines = append(lines, value)
			// SSE joins consecutive data lines of one frame with newlines.
			if len(payload) > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, value...)
		}
	}
	return eventType, payload, lines
}

// observeFrameParts reads what parseSSEFrame produced.
func (o *modelObserver) observeFrameParts(eventType string, payload []byte, lines [][]byte) bool {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	// A frame with data but no event line is a chat-completions style chunk;
	// treat it as non-terminal unless its payload says otherwise.
	if eventType == "" {
		eventType = "chunk"
	}
	if o.observePayload(payload, eventType) {
		return true
	}
	// The joined form is the spec, but a host that packs several events into
	// one frame leaves data lines that are each a payload of their own. Only
	// tried once the joined form has failed, so a conforming frame is never
	// read twice.
	if len(lines) < 2 {
		return false
	}
	parsed := false
	for _, line := range lines {
		if bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if o.observePayload(line, eventType) {
			parsed = true
		}
	}
	return parsed
}

// salvagedPayload is one JSON value recovered from unframed bytes, with the
// offset just past it so a caller can tell how much it consumed.
type salvagedPayload struct {
	payload []byte
	end     int
}

// salvageDataPayloads pulls every JSON value that follows a data: marker even
// when the newlines that should frame them are gone. A host that writes
// "event: response.created" and "data: {...}" with nothing between them leaves
// bytes the line-based parse cannot see at all: the whole run reads as one
// event line containing no data, and every model declaration in it is lost.
//
// Each value is taken by balancing brackets from the first { or [ after the
// marker, which is what makes the end of one event findable without the
// separator that should have marked it. A value that is still truncated
// yields nothing, so the caller keeps buffering instead of reading half of it.
func salvageDataPayloads(frame []byte) []salvagedPayload {
	var out []salvagedPayload
	marker := []byte("data:")
	for offset := 0; offset < len(frame); {
		index := bytes.Index(frame[offset:], marker)
		if index < 0 {
			break
		}
		start := offset + index + len(marker)
		value, end := firstJSONValue(frame[start:])
		if value == nil {
			offset = start
			continue
		}
		out = append(out, salvagedPayload{payload: value, end: start + end})
		offset = start + end
	}
	return out
}

// firstJSONValue returns the first balanced JSON object or array in data and
// the offset just past it. Strings and their escapes are respected, so a
// bracket inside a value never ends the scan early.
func firstJSONValue(data []byte) ([]byte, int) {
	start := -1
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		if c == '{' || c == '[' {
			start = i
		}
		break
	}
	if start < 0 {
		return nil, 0
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(data); i++ {
		c := data[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return data[start : i+1], i + 1
			}
		}
	}
	return nil, 0
}

// observeFrame reads one SSE frame whose boundaries are known, and reports
// whether any payload in it parsed.
func (o *modelObserver) observeFrame(frame []byte) bool {
	eventType, payload, lines := parseSSEFrame(frame)
	if o.observeFrameParts(eventType, payload, lines) {
		return true
	}
	parsed := false
	for _, salvaged := range salvageDataPayloads(frame) {
		if o.observePayload(salvaged.payload, "chunk") {
			parsed = true
		}
	}
	return parsed
}

// observeTail reads a trailing run that no separator ended and reports how many
// bytes it consumed. One complete frame is consumed whole; a run of frames the
// host concatenated without framing is consumed up to the end of the last
// complete payload, leaving a truncated one for the next chunk.
func (o *modelObserver) observeTail(data []byte) int {
	eventType, payload, lines := parseSSEFrame(data)
	if o.observeFrameParts(eventType, payload, lines) {
		return len(data)
	}
	consumed := 0
	for _, salvaged := range salvageDataPayloads(data) {
		o.observePayload(salvaged.payload, "chunk")
		consumed = salvaged.end
	}
	return consumed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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

// requestReasoningEffort reads the effort a request asked for. It is read from
// the outgoing payload rather than inferred, and read once: the body is not
// kept for this, only the short value is.
func requestReasoningEffort(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var asked struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(trimmed, &asked); err != nil {
		return ""
	}
	effort := strings.TrimSpace(asked.Reasoning.Effort)
	if len([]rune(effort)) > 32 {
		return ""
	}
	return effort
}
