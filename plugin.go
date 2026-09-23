package main

import (
	"encoding/json"
	"fmt"
)

func pluginRegistration() registration {
	return registration{SchemaVersion: schemaVersion, Metadata: metadata{Name: pluginName, Version: pluginVersion, Author: "tapaixx", GitHubRepository: "https://github.com/tapaixx/codex-header-rewrite", Logo: pluginLogo, ConfigFields: []configField{{Name: "data_path", Type: "string", Description: "bbolt data path. Defaults to plugins/data/codex-header-rewrite.db"}, {Name: "probe_degraded_markers", Type: "string", Description: "Automatic state probe: comma-separated markers. A probe response whose event names, event types or field names contain one is judged degraded and its state is not pooled. Empty: probes are not judged. Server configuration only; never shown in the panel."}}}, Capabilities: registrationCapabilities{RequestInterceptor: true, RequestLifecyclePlugin: true, ResponseInterceptor: true, StreamChunkInterceptor: true, ManagementAPI: true}}
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		if err := configurePlugin(raw); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case methodPluginQuiesce:
		if err := quiescePlugin(); err != nil {
			return nil, err
		}
		return okEnvelope(struct{}{})
	case methodRequestInterceptBefore:
		return okEnvelope(requestInterceptResponse{})
	case methodRequestInterceptAfter:
		var req requestInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		resp, err := interceptAfter(req)
		if err != nil {
			return nil, err
		}
		return okEnvelope(resp)
	case methodRequestComplete:
		var req requestCompletion
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		completeRequest(req)
		return okEnvelope(struct{}{})
	case methodResponseInterceptAfter:
		var req responseInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return okEnvelope(observeResponse(req))
	case methodResponseInterceptStreamChunk:
		var req streamChunkInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return okEnvelope(observeStreamHeaders(req))
	case methodManagementRegister:
		return okEnvelope(registerManagement())
	case methodManagementHandle:
		return handleManagement(raw)
	default:
		return errorEnvelope("unknown_method", fmt.Sprintf("unknown method: %s", method)), nil
	}
}
