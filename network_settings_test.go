package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNetworkSettingsAPIAndPersistence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	h := newHub(filepath.Join(dir, "account.json"))
	h.settings = &settingsAPI{path: cfgPath, active: defaultNetworkSettings()}
	restarted := 0
	h.settings.restart = func() error { restarted++; return nil }
	server := httptest.NewServer(h.routes())
	defer server.Close()
	alice := newPerson(t, server, "Alice")
	initial := testAPI(t, server, "GET", "/api/settings", alice, nil, http.StatusOK)
	settings := initial["settings"].(map[string]any)
	if settings["directoryHost"] != "" || settings["peerListen"] != "0.0.0.0:8788" {
		t.Fatalf("defaults: %v", settings)
	}
	cfg := defaultNetworkSettings()
	cfg.DirectoryHost = "directory.example.com"
	cfg.DirectoryTLS = true
	cfg.PeerAdvertise = "http://203.0.113.90:8788"
	cfg.STUN = "stun:stun.example.com:3478"
	testAPI(t, server, "PUT", "/api/settings", alice, cfg, http.StatusOK)
	loaded, err := loadNetworkSettings(cfgPath)
	if err != nil || loaded != cfg {
		t.Fatalf("settings did not persist: %v %+v", err, loaded)
	}
	info, err := os.Stat(cfgPath)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		t.Fatalf("settings permissions: %v, %v", info, err)
	}
	cfg.PeerListen = "invalid"
	testAPI(t, server, "PUT", "/api/settings", alice, cfg, http.StatusBadRequest)
	loaded, _ = loadNetworkSettings(cfgPath)
	if loaded.PeerListen != "0.0.0.0:8788" {
		t.Fatal("bad settings overwritten good settings")
	}
	testAPI(t, server, "PUT", "/api/settings", testIdentity{}, cfg, http.StatusUnauthorized)
	testAPI(t, server, "POST", "/api/settings/restart", alice, map[string]any{}, http.StatusAccepted)
	if restarted != 1 {
		t.Fatalf("restart called %d times", restarted)
	}
	testAPI(t, server, "POST", "/api/settings/restart", alice, map[string]any{}, http.StatusConflict)
}

func TestNetworkSettingsValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*NetworkSettings)
	}{
		{"wrong ports", func(c *NetworkSettings) { c.DirectoryPublishPort = 65536 }},
		{"bad hostname", func(c *NetworkSettings) { c.DirectoryHost = "https://user:password@somewhere" }},
		{"bad listen", func(c *NetworkSettings) { c.PeerListen = "0.0.0.0:-1" }},
		{"bad advertised URL", func(c *NetworkSettings) { c.PeerAdvertise = "https://public.example.com:8788" }},
		{"missing cert", func(c *NetworkSettings) { c.PeerTLSCert = "/tmp/cert.pem" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultNetworkSettings()
			tc.change(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatalf("expected failure for %+v", cfg)
			}
		})
	}
}

func TestDirectoryCodeJSONPreservesFullPrecision(t *testing.T) {
	h := newHub(filepath.Join(t.TempDir(), "account.json"))
	private, err := meshKey(filepath.Join(t.TempDir(), "nodekey"))
	if err != nil {
		t.Fatal(err)
	}
	mesh := newMesh(h, private)
	h.mesh = mesh
	mesh.directoryCode = (1 << 62) + 10023
	mesh.directory = &directoryClient{}
	s := httptest.NewServer(h.routes())
	defer s.Close()
	alice := newPerson(t, s, "Alice")
	result := testAPI(t, s, "GET", "/api/peers", alice, nil, http.StatusOK)
	got, ok := result["directoryCode"].(string)
	if !ok || got != "4611686018427397927" {
		t.Fatalf("code lost precision: %T %v", result["directoryCode"], result["directoryCode"])
	}
}

func TestSavedDiscoveryURLMigratesToDocumentedStream(t *testing.T) {
	cfg := defaultNetworkSettings()
	cfg.DirectoryHost = "http://soulless.ai/v/api/ghost"
	cfg.DirectoryPublishPort, cfg.DirectoryLookupPort, cfg.DirectoryTLS = 443, 443, false
	path := filepath.Join(t.TempDir(), "network.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadNetworkSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DirectoryHost != "www.soulless.ai" || loaded.DirectoryPublishPort != 443 || loaded.DirectoryLookupPort != 443 || loaded.DirectoryTLS {
		t.Fatalf("stale HTTP settings did not migrate to the supplied TLS stream recipe: %+v", loaded)
	}
}

func TestSavedDiscoverySettingsPreserveChosenPortAndTLS(t *testing.T) {
	for _, host := range []string{"www.soulless.ai/v/api/ghost", "www.soulless.ai"} {
		cfg := defaultNetworkSettings()
		cfg.DirectoryHost = host
		cfg.DirectoryPublishPort, cfg.DirectoryLookupPort, cfg.DirectoryTLS = 9443, 9443, false
		path := filepath.Join(t.TempDir(), "network.json")
		if err := saveNetworkSettings(path, cfg); err != nil {
			t.Fatal(err)
		}
		got, err := loadNetworkSettings(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != cfg {
			t.Fatalf("saved settings changed across restart: got %+v, want %+v", got, cfg)
		}
	}
}
