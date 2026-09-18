package main

import (
	"encoding/json"
	"net/http"
	"net/url"
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
	methodRequestInterceptBefore       = "request.intercept_before"
	methodRequestInterceptAfter        = "request.intercept_after"
	methodRequestComplete              = "request.complete"
	methodResponseInterceptAfter       = "response.intercept_after"
	methodResponseInterceptStreamChunk = "response.intercept_stream_chunk"
	methodManagementRegister           = "management.register"
	methodManagementHandle             = "management.handle"
	methodHostAuthList                 = "host.auth.list"
	methodHostAuthGetRuntime           = "host.auth.get_runtime"
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
