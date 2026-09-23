package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func headersOf(t *testing.T, doc json.RawMessage) (map[string]string, map[string]json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc, &fields); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{}
	if raw, ok := fields["headers"]; ok {
		if err := json.Unmarshal(raw, &headers); err != nil {
			t.Fatal(err)
		}
	}
	return headers, fields
}

// Only the plugin's own entry is added or removed; everything else in the
// file, and any header the operator set, is carried over as it was.
func TestEditCookieForwarding(t *testing.T) {
	base := json.RawMessage(`{"type":"codex","access_token":"tok","refresh_token":"ref","email":"a@example.com"}`)

	on, changed, err := editCookieForwarding(base, true)
	if err != nil || !changed {
		t.Fatalf("add: changed=%v err=%v", changed, err)
	}
	headers, fields := headersOf(t, on)
	if headers["Cookie"] != "$Cookie" || string(fields["refresh_token"]) != `"ref"` || string(fields["access_token"]) != `"tok"` {
		t.Fatalf("added: %s", on)
	}
	if _, changed, _ := editCookieForwarding(on, true); changed {
		t.Fatal("already present: nothing to do")
	}
	off, changed, err := editCookieForwarding(on, false)
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	if _, fields := headersOf(t, off); fields["headers"] != nil {
		t.Fatalf("an emptied headers object goes away: %s", off)
	}

	// The operator's other headers stay, both ways.
	mixed := json.RawMessage(`{"type":"codex","headers":{"X-Team":"a"}}`)
	on, _, _ = editCookieForwarding(mixed, true)
	if h, _ := headersOf(t, on); h["X-Team"] != "a" || h["Cookie"] != "$Cookie" {
		t.Fatalf("mixed add: %s", on)
	}
	off, _, _ = editCookieForwarding(on, false)
	if h, _ := headersOf(t, off); h["X-Team"] != "a" || h["Cookie"] != "" {
		t.Fatalf("mixed remove: %s", off)
	}

	// A Cookie header the operator set to something else is theirs.
	taken := json.RawMessage(`{"headers":{"cookie":"fixed=1"}}`)
	if _, _, err := editCookieForwarding(taken, true); err != errCookieHeaderTaken {
		t.Fatalf("a foreign Cookie header must not be overwritten: %v", err)
	}
	if _, changed, err := editCookieForwarding(taken, false); err != nil || changed {
		t.Fatalf("and must not be removed either: changed=%v err=%v", changed, err)
	}
	if _, _, err := editCookieForwarding(json.RawMessage(`{"headers":"x"}`), true); err == nil || !strings.Contains(err.Error(), "not an object") {
		t.Fatalf("a malformed headers field is refused: %v", err)
	}
}

// The file is re-read just before saving; a copy that changed in between --
// a token refresh -- is edited again rather than saved over.
func TestSetCookieForwardingRereadsBeforeSaving(t *testing.T) {
	oldGet, oldSave := hostAuthGetFileFunc, hostAuthSaveFunc
	t.Cleanup(func() { hostAuthGetFileFunc, hostAuthSaveFunc = oldGet, oldSave })
	docs := []string{`{"refresh_token":"old"}`, `{"refresh_token":"new"}`, `{"refresh_token":"new"}`, `{"refresh_token":"new"}`}
	reads := 0
	hostAuthGetFileFunc = func(string) (hostAuthFile, error) {
		doc := docs[min(reads, len(docs)-1)]
		reads++
		return hostAuthFile{Name: "a.json", JSON: json.RawMessage(doc)}, nil
	}
	var saved []hostAuthFile
	hostAuthSaveFunc = func(f hostAuthFile) error { saved = append(saved, f); return nil }

	if err := setCookieForwarding("idx", true); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Name != "a.json" {
		t.Fatalf("one save expected: %+v", saved)
	}
	var fields map[string]any
	_ = json.Unmarshal(saved[0].JSON, &fields)
	if fields["refresh_token"] != "new" {
		t.Fatalf("the refreshed token must survive: %s", saved[0].JSON)
	}

	// A file that never settles is left alone.
	saved, reads = nil, 0
	flip := 0
	hostAuthGetFileFunc = func(string) (hostAuthFile, error) {
		flip++
		return hostAuthFile{Name: "a.json", JSON: json.RawMessage(`{"n":` + string(rune('0'+flip%10)) + `}`)}, nil
	}
	if err := setCookieForwarding("idx", true); err == nil || len(saved) != 0 {
		t.Fatalf("a changing file must not be saved: err=%v saved=%d", err, len(saved))
	}
}
