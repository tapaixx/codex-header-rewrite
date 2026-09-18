package main

import (
	"encoding/json"
	"fmt"
)

func errHostCall(method, detail string) error {
	return fmt.Errorf("host callback %s: %s", method, detail)
}

func callHost(method string, payload any, out any) error {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal host payload: %w", err)
	}
	raw, err := callHostRaw(method, rawPayload)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode host envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("host callback failed")
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decode host result: %w", err)
	}
	return nil
}

func callHostAuthList() (hostAuthListResponse, error) {
	var out hostAuthListResponse
	err := callHost(methodHostAuthList, map[string]any{}, &out)
	return out, err
}
func callHostAuthGetRuntime(authIndex string) (hostAuthGetRuntimeResponse, error) {
	var out hostAuthGetRuntimeResponse
	err := callHost(methodHostAuthGetRuntime, hostAuthGetRequest{AuthIndex: authIndex}, &out)
	return out, err
}

// callHostAuthGet returns one credential document. The bytes contain
// authentication material: callers consume them for a single upstream call and
// never persist, log, or return them.
func callHostAuthGet(authIndex string) (json.RawMessage, error) {
	var out hostAuthGetResponse
	if err := callHost(methodHostAuthGet, hostAuthGetRequest{AuthIndex: authIndex}, &out); err != nil {
		return nil, err
	}
	document := out.document()
	if len(document) == 0 {
		return nil, errHostCall(methodHostAuthGet, "empty auth document")
	}
	return append(json.RawMessage(nil), document...), nil
}

func callHostHTTPDo(request hostHTTPRequest) (hostHTTPResponse, error) {
	var out hostHTTPResponse
	if err := callHost(methodHostHTTPDo, request, &out); err != nil {
		return hostHTTPResponse{}, err
	}
	return out, nil
}

var hostAuthListFunc = callHostAuthList
var hostAuthGetRuntimeFunc = callHostAuthGetRuntime
var hostAuthGetFunc = callHostAuthGet
var hostHTTPDoFunc = callHostHTTPDo
