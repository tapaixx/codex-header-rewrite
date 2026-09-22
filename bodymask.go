package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// A recorded body exists to explain what was sent and what came back around
// the conversation -- the model, the tools, the turn metadata, the reasoning
// settings, the error shape. The conversation itself is not part of that, so
// the field carrying it is replaced before the body is stored. What is left
// still says how big the masked value was, which is usually the only thing
// about it that matters when reading a request back.
var (
	// What a request asked is the conversation; what a response answered is the
	// conversation too. The tool schemas are neither -- they are the same
	// fifteen kilobytes of declarations on every single turn, and knowing the
	// tools were present says as much as reading them again.
	requestContentFields  = []string{"input"}
	responseContentFields = []string{"output", "tools"}
)

const (
	// A stream is accumulated raw so that masking sees whole SSE frames, then
	// cut to maxStoredBodyBytes once masked. Masking chunk by chunk would let a
	// frame split across two chunks through unmasked.
	maxStreamBufferBytes = 4 * maxStoredBodyBytes
)

func maskedValue(size int) string { return fmt.Sprintf("[MASKED %d bytes]", size) }

// maskRequestBody hides what was asked; maskResponseBody hides what was
// answered. Both walk the whole document, so a field nested under response or
// under an event is masked as well as one at the top level.
func maskRequestBody(body []byte) []byte  { return maskBodyFields(body, requestContentFields) }
func maskResponseBody(body []byte) []byte { return maskBodyFields(body, responseContentFields) }

// maskBodyFields replaces every value stored under any of the named fields. A
// payload that is not JSON is treated as an SSE stream and masked frame by
// frame; a payload that is neither is returned untouched rather than mangled.
func maskBodyFields(body []byte, fields []string) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if masked, ok := maskJSONDocument(trimmed, fields); ok {
			return masked
		}
		return body
	}
	return maskSSEBody(body, fields)
}

func maskJSONDocument(payload []byte, fields []string) ([]byte, bool) {
	var document any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, false
	}
	wanted := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		wanted[field] = struct{}{}
	}
	masked := maskJSONValue(document, wanted)
	out, err := json.Marshal(masked)
	if err != nil {
		return nil, false
	}
	return out, true
}

// maskJSONValue walks a decoded document and replaces the named fields wherever
// they appear. The replacement records the size the value serialised to, so a
// masked request can still be told apart from an empty one.
func maskJSONValue(value any, fields map[string]struct{}) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if _, masked := fields[key]; masked {
				typed[key] = maskedValue(jsonSize(nested))
				continue
			}
			typed[key] = maskJSONValue(nested, fields)
		}
		return typed
	case []any:
		for i, nested := range typed {
			typed[i] = maskJSONValue(nested, fields)
		}
		return typed
	default:
		return value
	}
}

func jsonSize(value any) int {
	raw, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(raw)
}

// keepsResponseFrame reports whether a streamed frame is kept for what it
// says. Three are:
//
//   - response.created carries the model, the reasoning settings, the tools
//     and the service tier -- the whole shape of what was asked.
//   - a terminal frame carries the declaration the model verdict prefers,
//     plus the final status and the usage.
//   - an error frame carries the upstream's own words, which are short and
//     the only account of what went wrong.
//
// Everything else -- in_progress, which merely repeats created, and the
// output_text.delta / output_item / content_part run -- is the answer being
// streamed. Keeping it would store the conversation one fragment at a time,
// which is the thing these bodies are not for: delta is not named output, so
// masking by field name never touched it.
func keepsResponseFrame(eventType string) bool {
	switch trimmed := strings.TrimSpace(eventType); trimmed {
	case "response.created":
		return true
	case "error", "response.error":
		return true
	default:
		return isTerminalModelEvent(trimmed)
	}
}

// maskedFrame replaces a frame's payload outright, keeping only its type so
// the sequence still reads as a sequence.
func maskedFrame(eventType string, size int) []byte {
	out, err := json.Marshal(map[string]any{"type": eventType, "masked": fmt.Sprintf("%d bytes", size)})
	if err != nil {
		return []byte(`{"masked":"error"}`)
	}
	return out
}

// payloadEventType reads the type a frame declares, which every Codex event
// carries; the event line is the fallback for hosts that omit it.
func payloadEventType(payload []byte, fromEventLine string) string {
	var declared struct {
		Type string `json:"type"`
		Data struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &declared); err == nil {
		if declared.Type != "" {
			return declared.Type
		}
		if declared.Data.Type != "" {
			return declared.Data.Type
		}
	}
	return strings.TrimSpace(fromEventLine)
}

// lastEventName reads the event type named just before a payload, from the
// framing that precedes it. The slice passed in only covers the text since the
// previous payload, so a name cannot leak forward into the next frame.
func lastEventName(lead []byte) string {
	index := bytes.LastIndex(lead, []byte("event:"))
	if index < 0 {
		return ""
	}
	rest := lead[index+len("event:"):]
	if cut := bytes.IndexAny(rest, "\r\n"); cut >= 0 {
		rest = rest[:cut]
	}
	if cut := bytes.Index(rest, []byte("data:")); cut >= 0 {
		rest = rest[:cut]
	}
	return string(bytes.TrimSpace(rest))
}

// maskSSEBody rewrites the payload of every event while leaving every other
// byte exactly where it was. A frame worth keeping has only its content field
// masked; every other frame loses its payload entirely.
//
// Payloads are located by balancing brackets from each data: marker rather
// than by splitting lines. A host that drops the framing sends the whole
// stream as one line, and a line-based pass matched nothing in it -- so the
// body would have been stored with every fragment of the answer in it, which
// is the opposite of what masking is for.
func maskSSEBody(body []byte, fields []string) []byte {
	var out bytes.Buffer
	cursor, scan := 0, 0
	for scan < len(body) {
		index := bytes.Index(body[scan:], []byte("data:"))
		if index < 0 {
			break
		}
		start := index + scan + len("data:")
		value, end := firstJSONValue(body[start:])
		if value == nil {
			scan = start
			continue
		}
		valueEnd := start + end
		if bytes.Equal(bytes.TrimSpace(value), []byte("[DONE]")) {
			scan = valueEnd
			continue
		}
		eventType := payloadEventType(value, lastEventName(body[cursor:start]))
		var rewritten []byte
		if keepsResponseFrame(eventType) || eventType == "" {
			// Unidentifiable frames are masked by field name rather than
			// discarded: one of them may be the only payload that declared
			// anything.
			masked, ok := maskJSONDocument(value, fields)
			if !ok {
				scan = valueEnd
				continue
			}
			rewritten = masked
		} else {
			rewritten = maskedFrame(eventType, len(value))
		}
		out.Write(body[cursor : valueEnd-len(value)])
		out.Write(rewritten)
		cursor, scan = valueEnd, valueEnd
	}
	out.Write(body[cursor:])
	return out.Bytes()
}
