package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// NetworkSettings contains only node networking configuration. Existing peers,
// identities, messages, and temporary Direct Connect invitations are untouched.
// Store is private (0600) because TURN credentials and TLS key paths may appear.
type NetworkSettings struct {
	DirectoryHost        string `json:"directoryHost"`
	DirectoryPublishPort int    `json:"directoryPublishPort"`
	DirectoryLookupPort  int    `json:"directoryLookupPort"`
	DirectoryTLS         bool   `json:"directoryTLS"`
	PeerListen           string `json:"peerListen"`
	PeerAdvertise        string `json:"peerAdvertise"`
	PeerTLSCert          string `json:"peerTLSCert"`
	PeerTLSKey           string `json:"peerTLSKey"`
	LAN                  bool   `json:"lan"`
	STUN                 string `json:"stun"`
	TURNURL              string `json:"turnURL"`
	TURNUser             string `json:"turnUser"`
	TURNPass             string `json:"turnPass"`
}

func defaultNetworkSettings() NetworkSettings {
	return NetworkSettings{DirectoryPublishPort: 443, DirectoryLookupPort: 443, DirectoryTLS: true, PeerListen: "0.0.0.0:8788", LAN: true}
}

func loadNetworkSettings(path string) (NetworkSettings, error) {
	cfg := defaultNetworkSettings()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid network settings: %w", err)
	}
	// Migrate saved URL-style configuration into a TCP hostname so an old
	// settings file cannot prevent the UI from starting to correct its port.
	if strings.Contains(cfg.DirectoryHost, "://") {
		raw := cfg.DirectoryHost
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
			cfg.DirectoryHost = u.Hostname()
			if (u.Hostname() == "soulless.ai" || u.Hostname() == "www.soulless.ai") && strings.TrimRight(u.Path, "/") == "/v/api/ghost" {
				cfg.DirectoryHost = "www.soulless.ai"
			}
		}
	}
	cfg.DirectoryLookupPort = cfg.DirectoryPublishPort
	if err = cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (cfg NetworkSettings) validate() error {
	cfg.DirectoryHost = strings.TrimSpace(cfg.DirectoryHost)
	if cfg.DirectoryHost != "" {
		if _, err := newDirectoryClient(cfg.DirectoryHost, cfg.DirectoryPublishPort, cfg.DirectoryLookupPort, cfg.DirectoryTLS); err != nil {
			return err
		}
	}
	if cfg.DirectoryPublishPort < 1 || cfg.DirectoryPublishPort > 65535 || cfg.DirectoryLookupPort < 1 || cfg.DirectoryLookupPort > 65535 {
		return errors.New("directory ports must be 1–65535")
	}
	host, port, err := net.SplitHostPort(cfg.PeerListen)
	if err != nil || host == "" || port == "" {
		return errors.New("peer listener must be host:port, e.g. 0.0.0.0:8788")
	}
	if p, err := net.LookupPort("tcp", port); err != nil || p < 1 {
		return errors.New("peer listening port must be 1–65535")
	}
	if cfg.PeerAdvertise != "" {
		url, err := safePeerURL(cfg.PeerAdvertise)
		if err != nil {
			return fmt.Errorf("public peer address: %w", err)
		}
		if (cfg.PeerTLSCert != "") != strings.HasPrefix(url, "https://") {
			return errors.New("public peer URL must be https when peer TLS is enabled, or http without it")
		}
	}
	if (cfg.PeerTLSCert == "") != (cfg.PeerTLSKey == "") {
		return errors.New("provide both peer TLS certificate and key paths")
	}
	if cfg.PeerTLSCert != "" {
		if _, err := tls.LoadX509KeyPair(cfg.PeerTLSCert, cfg.PeerTLSKey); err != nil {
			return fmt.Errorf("peer TLS certificate/key: %w", err)
		}
	}
	if len(cfg.STUN) > 2048 || len(cfg.TURNURL) > 2048 || len(cfg.TURNUser) > 256 || len(cfg.TURNPass) > 1024 {
		return errors.New("ICE settings are too long")
	}
	return nil
}

func saveNetworkSettings(path string, cfg NetworkSettings) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ghost-network-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type settingsAPI struct {
	path     string
	active   NetworkSettings
	stopping atomic.Bool
	restart  func() error // injected so tests never spawn processes
}

func (s *settingsAPI) handler(w http.ResponseWriter, r *http.Request, h *Hub) {
	if h.authenticate(w, r) == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		sendJSON(w, 200, map[string]any{"settings": s.active, "restartRequired": false})
	case http.MethodPut:
		var cfg NetworkSettings
		if !decodeBody(w, r, &cfg) {
			return
		}
		cfg.DirectoryHost = strings.TrimSpace(cfg.DirectoryHost)
		cfg.PeerListen = strings.TrimSpace(cfg.PeerListen)
		cfg.PeerAdvertise = strings.TrimSpace(cfg.PeerAdvertise)
		if err := saveNetworkSettings(s.path, cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sendJSON(w, 200, map[string]any{"saved": true, "restartRequired": cfg != s.active})
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "Method not allowed", 405)
	}
}

func (s *settingsAPI) restartHandler(w http.ResponseWriter, r *http.Request, h *Hub) {
	if !method(w, r, http.MethodPost) || h.authenticate(w, r) == nil {
		return
	}
	if !s.stopping.CompareAndSwap(false, true) {
		http.Error(w, "Restart already requested", 409)
		return
	}
	if s.restart == nil {
		s.stopping.Store(false)
		http.Error(w, "Restart unavailable", 503)
		return
	}
	if err := s.restart(); err != nil {
		s.stopping.Store(false)
		http.Error(w, err.Error(), 500)
		return
	}
	sendJSON(w, 202, map[string]bool{"restarting": true})
}

// Queue a new native binary after responding. The child waits before binding,
// allowing the original listeners to close; the browser tab stays open.
func launchDesktopRestart(data, listen string, tray bool, srv *http.Server, mesh *Mesh) error {
	if runtime.GOOS == "android" {
		return errors.New("Android client does not run a local Ghost node")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"-data", data, "-listen", listen, "-open=false", fmt.Sprintf("-tray=%t", tray)}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "GHOST_RESTARTING=1")
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	go func() {
		time.Sleep(300 * time.Millisecond)
		if mesh != nil {
			mesh.close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		os.Exit(0)
	}()
	return nil
}

func (s *settingsAPI) testDirectory(w http.ResponseWriter, r *http.Request, h *Hub) {
	if !method(w, r, http.MethodPost) || h.authenticate(w, r) == nil {
		return
	}
	var cfg struct {
		Host      string `json:"host"`
		TLS       bool   `json:"tls"`
		Publish   int    `json:"publish"`
		Lookup    int    `json:"lookup"`
		Advertise string `json:"advertise"`
	}
	if !decodeBody(w, r, &cfg) {
		return
	}
	ctx, trace := h.diagnostics.begin(r.Context(), "directory.test")
	if cfg.Lookup == 0 {
		cfg.Lookup = cfg.Publish
	}
	trace.event("directory.config", "start", "Validating discovery endpoint")
	dir, err := newDirectoryClient(cfg.Host, cfg.Publish, cfg.Lookup, cfg.TLS)
	if err != nil {
		_ = trace.fail("directory.config", err)
		http.Error(w, err.Error(), 400)
		return
	}
	dir.diagnostics = h.diagnostics
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	probeID, err := randomID(26)
	if err != nil {
		http.Error(w, trace.fail("directory.probe", err).Error(), 500)
		return
	}
	_, err = dir.do(ctx, directoryRequest{Op: "get", Key: "ghost:probe:" + probeID})
	if err != nil && !directoryMissing(err) {
		http.Error(w, "Discovery get failed: "+err.Error(), 502)
		return
	}
	message := "TCP/TLS connection and JSONL get verified. Set a public peer URL to test signed publication and readback."
	if cfg.Advertise != "" && h.mesh != nil {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			http.Error(w, trace.fail("directory.probe", err).Error(), 500)
			return
		}
		id := directoryID(public)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			if _, err := dir.do(cleanup, directoryRequest{Op: "delete", Key: directoryKey(id)}); err != nil {
				trace.event("directory.probe.cleanup", "warning", "Temporary probe could not be removed; the server lease will expire it")
			}
		}()
		_, err = dir.publish(ctx, private, strings.TrimSpace(cfg.Advertise))
		if err != nil {
			http.Error(w, "Discovery set failed: "+err.Error(), 502)
			return
		}
		endpoint, key, err := dir.resolve(ctx, id)
		record, packErr := packDirectoryRecord(private, strings.TrimSpace(cfg.Advertise), uint32(time.Now().Unix()))
		expected, _, unpackErr := unpackDirectoryRecord(record, id)
		if err != nil {
			http.Error(w, "Discovery readback failed: "+err.Error(), 502)
			return
		}
		if packErr != nil || unpackErr != nil || endpoint != expected || key != base64.RawURLEncoding.EncodeToString(public) {
			http.Error(w, trace.fail("directory.readback", errors.New("signed peer advertisement readback does not match this node")).Error(), 502)
			return
		}
		message = "Signed publication and lookup verified using JSONL set/get over TCP. Connect with the directory code to verify the remote peer path."
	}
	trace.event("directory.test", "ok", message)
	sendJSON(w, 200, map[string]any{"reachable": true, "message": message, "attempt": trace.id})
}

func (s *settingsAPI) detectIP(w http.ResponseWriter, r *http.Request, h *Hub) {
	if !method(w, r, http.MethodPost) || h.authenticate(w, r) == nil {
		return
	}
	// Explicit UI opt-in: this contacts a third-party service; never call on startup.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org", nil)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	transport := networkHTTPTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		http.Error(w, "Public-IP detection failed: "+err.Error(), 502)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		http.Error(w, "IP service unavailable", 502)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 80))
	ip := net.ParseIP(strings.TrimSpace(string(raw)))
	if err != nil || ip == nil {
		http.Error(w, "Invalid IP response", 502)
		return
	}
	sendJSON(w, 200, map[string]string{"ip": ip.String(), "warning": "This may be the router or CGNAT public IP; you may still need port forwarding."})
}
