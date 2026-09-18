//go:build !localtest

package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestPluginQuiesceFlushesHistoryAndReleasesTheStoreLock(t *testing.T) {
	shutdownPlugin()
	dataPath := filepath.Join(t.TempDir(), "hot-reload.db")
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("data_path: " + dataPath)})
	if err != nil {
		t.Fatal(err)
	}
	if err := configurePlugin(request); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdownPlugin)

	state.mu.Lock()
	writer := state.writer
	state.mu.Unlock()
	writer.Enqueue(historyRecord{
		ID:        "before-reload#1",
		AuthIndex: "idx-a",
		StartedAt: time.Now().UTC(),
		Outcome:   "succeeded",
	})

	response, err := handleMethod("plugin.quiesce", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result envelope
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("plugin.quiesce response=%s", response)
	}

	reopened := make(chan struct {
		store persistence
		err   error
	}, 1)
	go func() {
		store, openErr := openPersistence(dataPath)
		reopened <- struct {
			store persistence
			err   error
		}{store: store, err: openErr}
	}()

	select {
	case opened := <-reopened:
		if opened.err != nil {
			t.Fatal(opened.err)
		}
		page, err := opened.store.History("idx-a", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != "before-reload#1" {
			t.Fatalf("quiesce did not flush queued history: %#v", page.Items)
		}
		if err := opened.store.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		shutdownPlugin()
		t.Fatal("the old plugin still holds the bbolt lock after plugin.quiesce")
	}

	if err := configurePlugin(request); err != nil {
		t.Fatalf("the quiesced instance could not be configured again after rollback: %v", err)
	}
}
