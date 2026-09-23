package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// CPA's Codex executor builds the upstream request from a fixed list of client
// headers, and Cookie is not on it: a Cookie the plugin injects never leaves
// CPA. The one way through is a custom header on the credential itself. CPA
// reads an auth file's "headers" object, and a value of "$Name" copies the
// request header Name -- as it stands after the plugin has rewritten it. So
// while cookie injection is in effect, the credential's file carries
//
//	"headers": {"Cookie": "$Cookie"}
//
// and the plugin puts it there and takes it away. Only that exact entry is
// ever added or removed; any other custom header, or a Cookie header set to
// something else, is the operator's and is left alone.

const cookieForwardValue = "$Cookie"

// hostAuthFile is one credential file as the host hands it over: its name,
// for saving it back, and its bytes. The bytes are credential material and
// are never logged, stored, or returned.
type hostAuthFile struct {
	Name string
	JSON json.RawMessage
}

func callHostAuthGetFile(authIndex string) (hostAuthFile, error) {
	var out hostAuthGetResponse
	if err := callHost(methodHostAuthGet, hostAuthGetRequest{AuthIndex: authIndex}, &out); err != nil {
		return hostAuthFile{}, err
	}
	document := out.document()
	if len(document) == 0 {
		return hostAuthFile{}, errHostCall(methodHostAuthGet, "empty auth document")
	}
	name := strings.TrimSpace(out.Name)
	if !strings.HasSuffix(strings.ToLower(name), ".json") && out.Path != "" {
		name = filepath.Base(out.Path)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		return hostAuthFile{}, errHostCall(methodHostAuthGet, "the credential has no auth file name")
	}
	return hostAuthFile{Name: name, JSON: append(json.RawMessage(nil), document...)}, nil
}

func callHostAuthSave(file hostAuthFile) error {
	return callHost(methodHostAuthSave, map[string]any{"name": file.Name, "json": file.JSON}, nil)
}

var hostAuthGetFileFunc = callHostAuthGetFile
var hostAuthSaveFunc = callHostAuthSave

// cookieForwardingWanted is whether the credential's file should carry the
// entry: exactly while cookie injection is in effect.
func cookieForwardingWanted(rule headerRule, exists bool) bool {
	return exists && rule.InjectCookie && rule.poolMaintained()
}

var errCookieHeaderTaken = errors.New("the auth file already sets a Cookie custom header to something else; remove it there first")

// setCookieForwarding adds or removes the entry in one credential's auth file.
// The file is read, edited, and read again just before saving: CPA rewrites
// the same file when it refreshes the token, and saving an edit of a copy that
// has since changed would put a spent refresh token back. A file that keeps
// changing is left alone and reported.
func setCookieForwarding(authIndex string, want bool) error {
	for try := 0; try < 3; try++ {
		file, err := hostAuthGetFileFunc(authIndex)
		if err != nil {
			return err
		}
		edited, changed, err := editCookieForwarding(file.JSON, want)
		if err != nil || !changed {
			return err
		}
		again, err := hostAuthGetFileFunc(authIndex)
		if err != nil {
			return err
		}
		if !bytes.Equal(again.JSON, file.JSON) {
			continue
		}
		return hostAuthSaveFunc(hostAuthFile{Name: file.Name, JSON: edited})
	}
	return errors.New("the auth file kept changing while it was being edited; try again")
}

// editCookieForwarding returns the document with the entry present or absent,
// and whether anything changed. Every other field is carried over as it was.
func editCookieForwarding(document json.RawMessage, want bool) (json.RawMessage, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return nil, false, err
	}
	headers := map[string]json.RawMessage{}
	if raw, ok := fields["headers"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &headers); err != nil {
			return nil, false, errors.New("the auth file's headers field is not an object")
		}
	}
	key, current, found := "", "", false
	for name, raw := range headers {
		if strings.EqualFold(name, "Cookie") {
			key, found = name, true
			_ = json.Unmarshal(raw, &current)
			break
		}
	}
	ours := found && strings.TrimSpace(current) == cookieForwardValue
	switch {
	case want && ours, !want && !ours:
		return document, false, nil
	case want && found:
		return nil, false, errCookieHeaderTaken
	case want:
		value, _ := json.Marshal(cookieForwardValue)
		headers["Cookie"] = value
	default:
		delete(headers, key)
	}
	if len(headers) == 0 {
		delete(fields, "headers")
	} else {
		raw, err := json.Marshal(headers)
		if err != nil {
			return nil, false, err
		}
		fields["headers"] = raw
	}
	edited, err := json.Marshal(fields)
	if err != nil {
		return nil, false, err
	}
	return edited, true, nil
}
