package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNetworkDiagnosticsAuthorizationBoundsAndRotation(t *testing.T) {
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	path := filepath.Join(t.TempDir(), "network.log")
	if err := h.diagnostics.enableFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 2*1024*1024), 0600); err != nil {
		t.Fatal(err)
	}
	_, trace := h.diagnostics.begin(context.Background(), "test")
	trace.event("test", "ok", "rotation")
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("log was not rotated", err)
	}
	h.diagnostics.mu.Lock()
	h.diagnostics.path = ""
	h.diagnostics.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 75; j++ {
				trace.event("test", "ok", "concurrent entry")
			}
		}()
	}
	wg.Wait()
	if len(h.diagnostics.events) != 500 {
		t.Fatalf("unbounded ring: %d", len(h.diagnostics.events))
	}
	for i := 1; i < len(h.diagnostics.events); i++ {
		if h.diagnostics.events[i].Sequence <= h.diagnostics.events[i-1].Sequence {
			t.Fatal("event ordering lost")
		}
	}
	s := httptest.NewServer(h.routes())
	defer s.Close()
	alice := newPerson(t, s, "Alice")
	testAPI(t, s, "GET", "/api/network/logs", testIdentity{}, nil, 401)
	testAPI(t, s, "POST", "/api/network/logs", testIdentity{}, map[string]string{"stage": "webrtc.data", "state": "open"}, 401)
	testAPI(t, s, "POST", "/api/network/logs", alice, map[string]string{"stage": "webrtc.data", "state": "open"}, 200)
	testAPI(t, s, "POST", "/api/network/logs", alice, map[string]string{"stage": "webrtc.data", "state": "secret-message-content"}, 400)
	got := testAPI(t, s, "GET", "/api/network/logs", alice, nil, 200)
	encoded, _ := json.Marshal(got)
	if bytes.Contains(encoded, []byte(alice.Token)) || bytes.Contains(encoded, []byte("secret-message-content")) {
		t.Fatal("sensitive content in diagnostics")
	}
}

func TestDiagnosticErrorsRedactRejectedURLCredentials(t *testing.T) {
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	path := filepath.Join(t.TempDir(), "network.log")
	if err := h.diagnostics.enableFile(path); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	m := newMesh(h, key)
	if err := m.connectPinned(context.Background(), "http://review-user:review-secret@example.invalid:notaport", "", true); err == nil {
		t.Fatal("expected URL validation error")
	}
	_, trace := h.diagnostics.begin(context.Background(), "test")
	trace.event("test", "error", "http://review-user:review-secret@example.invalid:80 and GHOST1.private-invite-code")
	data, _ := json.Marshal(h.diagnostics.events)
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{string(data), string(disk)} {
		if strings.Contains(value, "review-secret") || strings.Contains(value, "private-invite-code") {
			t.Fatal("credentials or private invitation leaked into diagnostics")
		}
	}
}
