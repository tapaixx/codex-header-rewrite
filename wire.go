package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	abiVersion                 uint32 = 1
	schemaVersion              uint32 = 6
	streamChunkHeaderInitIndex        = -1
)

const (
	methodPluginRegister               = "plugin.register"
	methodPluginReconfigure            = "plugin.reconfigure"
	methodPluginQuiesce                = "plugin.quiesce"
	methodRequestInterceptBefore       = "request.intercept_before"
	methodRequestInterceptAfter        = "request.intercept_after"
	methodRequestComplete              = "request.complete"
	methodResponseInterceptAfter       = "response.intercept_after"
	methodResponseInterceptStreamChunk = "response.intercept_stream_chunk"
	methodManagementRegister           = "management.register"
	methodManagementHandle             = "management.handle"
	methodHostAuthList                 = "host.auth.list"
	methodHostAuthGetRuntime           = "host.auth.get_runtime"
	methodHostAuthGet                  = "host.auth.get"
	methodHostHTTPDo                   = "host.http.do"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	ConfigFields     []configField
}

type configField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      metadata                 `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
	ManagementAPI          bool `json:"management_api"`
}

type requestInterceptRequest struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	ToFormat       string
	Model          string
	RequestedModel string
	Stream         bool
	Headers        http.Header
	Body           []byte
	Metadata       map[string]any
}

type requestInterceptResponse struct {
	Headers         http.Header
	Body            []byte
	ClearHeaders    []string
	Terminate       bool
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte
}

type requestCompletion struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
	Metadata       map[string]any
}

type responseInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	StatusCode      int
	Metadata        map[string]any
}

type responseInterceptResponse struct {
	Headers      http.Header
	Body         []byte
	ClearHeaders []string
}

type streamChunkInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	HistoryChunks   [][]byte
	ChunkIndex      int
	Metadata        map[string]any
}

type streamChunkInterceptResponse struct {
	Headers      http.Header
	Body         []byte
	ClearHeaders []string
}

type managementRegistration struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type hostAuthFileEntry struct {
	ID            string    `json:"id,omitempty"`
	AuthIndex     string    `json:"auth_index,omitempty"`
	Name          string    `json:"name"`
	Type          string    `json:"type,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	Label         string    `json:"label,omitempty"`
	Status        string    `json:"status,omitempty"`
	StatusMessage string    `json:"status_message,omitempty"`
	Disabled      bool      `json:"disabled,omitempty"`
	Unavailable   bool      `json:"unavailable,omitempty"`
	RuntimeOnly   bool      `json:"runtime_only,omitempty"`
	Source        string    `json:"source,omitempty"`
	Path          string    `json:"path,omitempty"`
	Size          int64     `json:"size,omitempty"`
	ModTime       time.Time `json:"modtime,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitempty"`
}

type hostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type hostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

type hostAuthGetRuntimeResponse struct {
	Auth hostAuthFileEntry `json:"auth"`
}

// hostAuthGetResponse carries one credential document. Hosts have used all
// three field names for the payload, so each is accepted.
type hostAuthGetResponse struct {
	JSON json.RawMessage `json:"json"`
	Auth json.RawMessage `json:"auth"`
	Data json.RawMessage `json:"data"`
}

func (r hostAuthGetResponse) document() json.RawMessage {
	for _, candidate := range []json.RawMessage{r.JSON, r.Auth, r.Data} {
		if len(candidate) > 0 {
			return candidate
		}
	}
	return nil
}

// hostHTTPRequest is a host-mediated outbound request. Body is base64 encoded
// by encoding/json, matching the host ABI's byte-buffer convention.
//
// The host performs the call with its own proxy-aware client, so the request
// leaves from CLIProxyAPI rather than from a second HTTP stack inside the
// plugin. WireProfile can make the HTTP/1.1 header layout deterministic; it
// does not claim to reproduce the Codex CLI's complete TLS fingerprint.
type hostHTTPRequest struct {
	Method      string           `json:"method"`
	URL         string           `json:"url"`
	Headers     http.Header      `json:"headers,omitempty"`
	Body        []byte           `json:"body,omitempty"`
	WireProfile *hostWireProfile `json:"wire_profile,omitempty"`
}

type hostWireProfile struct {
	HTTP1Only              bool     `json:"http1_only,omitempty"`
	DisableAutoCompression bool     `json:"disable_auto_compression,omitempty"`
	HeaderProfile          []string `json:"header_profile,omitempty"`
}

type hostHTTPResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       []byte      `json:"body,omitempty"`
}

// UnmarshalJSON accepts snake_case and Go-style casing, header maps of either
// slices or scalars, and a body that is either base64 (the ABI convention) or
// literal text (used by host test doubles).
func (r *hostHTTPResponse) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	out := hostHTTPResponse{}
	if raw := firstRawField(object, "status_code", "statusCode", "StatusCode"); len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.StatusCode); err != nil {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return fmt.Errorf("invalid host HTTP status: %w", err)
			}
			parsed, convErr := strconv.Atoi(strings.TrimSpace(text))
			if convErr != nil {
				return fmt.Errorf("invalid host HTTP status: %w", convErr)
			}
			out.StatusCode = parsed
		}
	}
	if raw := firstRawField(object, "headers", "Headers"); len(raw) > 0 && string(raw) != "null" {
		var slices map[string][]string
		if err := json.Unmarshal(raw, &slices); err == nil {
			out.Headers = http.Header(slices)
		} else {
			var scalars map[string]string
			if err := json.Unmarshal(raw, &scalars); err != nil {
				return fmt.Errorf("invalid host HTTP headers: %w", err)
			}
			out.Headers = make(http.Header, len(scalars))
			for key, value := range scalars {
				out.Headers[key] = []string{value}
			}
		}
	}
	if raw := firstRawField(object, "body", "Body"); len(raw) > 0 && string(raw) != "null" {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err == nil {
			if decoded, decodeErr := base64.StdEncoding.DecodeString(encoded); decodeErr == nil {
				out.Body = decoded
			} else {
				out.Body = []byte(encoded)
			}
		} else if err := json.Unmarshal(raw, &out.Body); err != nil {
			return fmt.Errorf("invalid host HTTP body: %w", err)
		}
	}
	*r = out
	return nil
}

func firstRawField(object map[string]json.RawMessage, names ...string) json.RawMessage {
	for _, name := range names {
		if raw, ok := object[name]; ok {
			return raw
		}
	}
	return nil
}
