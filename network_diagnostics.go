package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Keep diagnostic metadata only: never record credentials, protocol bodies,
// invite codes, SDP, ICE candidates, chat content, or file data.
type networkEvent struct {
	Sequence  uint64 `json:"sequence"`
	Time      string `json:"time"`
	Attempt   uint64 `json:"attempt"`
	Stage     string `json:"stage"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	ElapsedMS int64  `json:"elapsedMs"`
}

type networkDiagnostics struct {
	mu                 sync.Mutex
	sequence, attempts uint64
	events             []networkEvent
	path               string
}

type networkTrace struct {
	d     *networkDiagnostics
	id    uint64
	start time.Time
}
type networkTraceKey struct{}

var diagnosticUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s"<>]*@`)
var diagnosticInvite = regexp.MustCompile(`GHOST1\.[A-Za-z0-9._-]+`)

func redactNetworkDetail(detail string) string {
	detail = diagnosticUserinfo.ReplaceAllString(detail, "${1}[redacted]@")
	return diagnosticInvite.ReplaceAllString(detail, "[redacted invite]")
}

func (d *networkDiagnostics) begin(ctx context.Context, stage string) (context.Context, *networkTrace) {
	if trace, ok := ctx.Value(networkTraceKey{}).(*networkTrace); ok {
		return ctx, trace
	}
	d.mu.Lock()
	d.attempts++
	id := d.attempts
	d.mu.Unlock()
	t := &networkTrace{d: d, id: id, start: time.Now()}
	t.event(stage, "start", "Connection attempt started")
	return context.WithValue(ctx, networkTraceKey{}, t), t
}

func (t *networkTrace) event(stage, status, detail string) {
	if t == nil {
		return
	}
	detail = redactNetworkDetail(detail)
	// Bound each entry even when a remote error contains arbitrary text.
	if len(detail) > 1024 {
		detail = detail[:1024]
	}
	d := t.d
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sequence++
	e := networkEvent{d.sequence, time.Now().UTC().Format(time.RFC3339Nano), t.id, stage, status, detail, time.Since(t.start).Milliseconds()}
	if len(d.events) == 500 {
		copy(d.events, d.events[1:])
		d.events = d.events[:499]
	}
	d.events = append(d.events, e)
	b, _ := json.Marshal(e)
	log.Printf("Ghost network: %s", b)
	if d.path != "" {
		if info, err := os.Stat(d.path); err == nil && info.Size() >= 2*1024*1024 {
			_ = os.Remove(d.path + ".1")
			if err := os.Rename(d.path, d.path+".1"); err != nil {
				log.Printf("Rotate network log: %v", err)
				return
			}
		}
		f, err := os.OpenFile(d.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			log.Printf("Write network log: %v", err)
			return
		}
		_, err = f.Write(append(b, '\n'))
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			log.Printf("Write network log: %v", err)
		}
	}
}

func (t *networkTrace) fail(stage string, err error) error {
	wrapped := fmt.Errorf("%s: %w", stage, err)
	detail := err.Error()
	var parseError *url.Error
	if errors.As(err, &parseError) {
		detail = parseError.Op + ": " + parseError.Err.Error()
	}
	t.event(stage, "error", detail)
	return wrapped
}

func (d *networkDiagnostics) enableFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	d.mu.Lock()
	d.path = path
	d.mu.Unlock()
	return nil
}

func (h *Hub) networkLogs(w http.ResponseWriter, r *http.Request) {
	if h.authenticate(w, r) == nil {
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			Stage string `json:"stage"`
			State string `json:"state"`
		}
		if !decodeBody(w, r, &input) {
			return
		}
		// A finite vocabulary prevents browser payloads from leaking into logs.
		allowed := map[string]map[string]bool{
			"webrtc.connection": {"new": true, "connecting": true, "connected": true, "disconnected": true, "failed": true, "closed": true},
			"webrtc.ice":        {"new": true, "checking": true, "connected": true, "completed": true, "disconnected": true, "failed": true, "closed": true},
			"webrtc.data":       {"open": true, "closed": true, "error": true},
		}
		if !allowed[input.Stage][input.State] {
			http.Error(w, "Invalid network event", 400)
			return
		}
		_, trace := h.diagnostics.begin(r.Context(), "browser")
		status := "ok"
		if input.State == "failed" || input.State == "error" {
			status = "error"
		}
		trace.event(input.Stage, status, input.State)
		sendJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	h.diagnostics.mu.Lock()
	events := append([]networkEvent{}, h.diagnostics.events...)
	path := h.diagnostics.path
	h.diagnostics.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	sendJSON(w, 200, map[string]any{"events": events, "file": path})
}
