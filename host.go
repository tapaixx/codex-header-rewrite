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

var hostAuthListFunc = callHostAuthList
var hostAuthGetRuntimeFunc = callHostAuthGetRuntime
