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
const (
	requestContentField  = "input"
	responseContentField = "output"
	// A stream is accumulated raw so that masking sees whole SSE frames, then
	// cut to maxStoredBodyBytes once masked. Masking chunk by chunk would let a
	// frame split across two chunks through unmasked.
	maxStreamBufferBytes = 4 * maxStoredBodyBytes
)

func maskedValue(size int) string { return fmt.Sprintf("[MASKED %d bytes]", size) }

// maskRequestBody hides what was asked; maskResponseBody hides what was
// answered. Both walk the whole document, so a field nested under response or
// under an event is masked as well as one at the top level.
func maskRequestBody(body []byte) []byte  { return maskBodyField(body, requestContentField) }
func maskResponseBody(body []byte) []byte { return maskBodyField(body, responseContentField) }

// maskBodyField replaces every value stored under the named field. A payload
// that is not JSON is treated as an SSE stream and masked frame by frame; a
// payload that is neither is returned untouched rather than mangled.
func maskBodyField(body []byte, field string) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if masked, ok := maskJSONDocument(trimmed, field); ok {
			return masked
		}
		return body
	}
	return maskSSEBody(body, field)
}

func maskJSONDocument(payload []byte, field string) ([]byte, bool) {
	var document any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, false
	}
	masked := maskJSONValue(document, field)
	out, err := json.Marshal(masked)
	if err != nil {
		return nil, false
	}
	return out, true
}

// maskJSONValue walks a decoded document and replaces the named field wherever
// it appears. The replacement records the size the value serialised to, so a
// masked request can still be told apart from an empty one.
func maskJSONValue(value any, field string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == field {
				typed[key] = maskedValue(jsonSize(nested))
				continue
			}
			typed[key] = maskJSONValue(nested, field)
		}
		return typed
	case []any:
		for i, nested := range typed {
			typed[i] = maskJSONValue(nested, field)
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

// maskSSEBody masks the payload of every data line while leaving the framing
// as it was, so the recorded stream still reads as the stream it was.
func maskSSEBody(body []byte, field string) []byte {
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		carriage := bytes.HasSuffix(line, []byte("\r"))
		bare := bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(bare, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bare[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		masked, ok := maskJSONDocument(payload, field)
		if !ok {
			continue
		}
		rebuilt := append([]byte("data: "), masked...)
		if carriage {
			rebuilt = append(rebuilt, '\r')
		}
		lines[i] = rebuilt
	}
	return bytes.Join(lines, []byte("\n"))
}

// maskedBodyNote describes the masking for the panel, so a reader is never
// left wondering whether a body was empty or withheld.
func maskedBodyNote(field string) string {
	return strings.ToUpper(field[:1]) + field[1:]
}
