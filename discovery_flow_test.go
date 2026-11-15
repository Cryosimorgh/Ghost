package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func jsonLine(value any) []byte { b, _ := json.Marshal(value); return append(b, '\n') }
func futureExpiry() float64     { return float64(time.Now().UnixNano())/1e9 + 600 }

// A local fixture implementing the supplied service's JSONL contract, with
// real certificate verification in TLS cases and no HTTP layer anywhere.
func startJSONLServer(t *testing.T, secure bool, handle func(directoryRequest) []byte) *directoryClient {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var listener net.Listener = raw
	host := "127.0.0.1"
	var roots *x509.CertPool
	if secure {
		host = "localhost"
		pub, key, _ := ed25519.GenerateKey(rand.Reader)
		template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		roots = x509.NewCertPool()
		roots.AddCert(cert)
		listener = tls.NewListener(raw, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	}
	port := raw.Addr().(*net.TCPAddr).Port
	d, err := newDirectoryClient(host, port, port, secure)
	if err != nil {
		t.Fatal(err)
	}
	if secure {
		d.tlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	var mu sync.Mutex
	active := map[net.Conn]bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			active[conn] = true
			mu.Unlock()
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				defer func() { mu.Lock(); delete(active, c); mu.Unlock() }()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				line, err := bufio.NewReaderSize(c, 4096).ReadSlice('\n')
				if err != nil {
					return
				}
				if !bytes.HasPrefix(line, []byte("{")) || line[len(line)-1] != '\n' {
					t.Errorf("request was not JSONL: first byte %x", line[0])
					return
				}
				if secure && c.(*tls.Conn).ConnectionState().ServerName != "localhost" {
					t.Error("incorrect TLS SNI")
				}
				var request directoryRequest
				if json.Unmarshal(line, &request) != nil {
					t.Error("invalid JSONL request")
					return
				}
				response := handle(request)
				// Exercise replies split across many writes, rather than one read == one reply.
				for len(response) > 0 {
					n := min(len(response), 7)
					if _, err := c.Write(response[:n]); err != nil {
						return
					}
					response = response[n:]
				}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for c := range active {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	d.networkTime = func(context.Context) (time.Time, error) { return time.Now(), nil }
	return d
}

func opaqueStore(ttl time.Duration, onSet func()) func(directoryRequest) []byte {
	type entry struct {
		value   string
		expires float64
	}
	values := map[string]entry{}
	var mu sync.Mutex
	return func(r directoryRequest) []byte {
		mu.Lock()
		defer mu.Unlock()
		switch r.Op {
		case "set":
			if onSet != nil {
				onSet()
			}
			e := entry{r.Value, float64(time.Now().UnixNano())/1e9 + ttl.Seconds()}
			values[r.Key] = e
			return jsonLine(map[string]any{"ok": true, "key": r.Key, "expires": e.expires})
		case "get":
			e, ok := values[r.Key]
			if !ok {
				return jsonLine(map[string]any{"ok": false, "error": "not found"})
			}
			if e.expires <= float64(time.Now().UnixNano())/1e9 {
				return jsonLine(map[string]any{"ok": false, "error": "expired"})
			}
			return jsonLine(map[string]any{"ok": true, "key": r.Key, "value": e.value, "expires": e.expires})
		case "delete":
			_, ok := values[r.Key]
			delete(values, r.Key)
			return jsonLine(map[string]any{"ok": true, "key": r.Key, "deleted": ok})
		default:
			return jsonLine(map[string]any{"ok": false, "error": "unknown op"})
		}
	}
}

func startPythonDirectory(t *testing.T) (*directoryClient, func()) {
	t.Helper()
	python := os.Getenv("GHOST_TEST_PYTHON")
	if python == "" {
		python, _ = exec.LookPath("python")
		if python == "" {
			python, _ = exec.LookPath("python3")
		}
	}
	if python == "" {
		t.Skip("Python not installed; set GHOST_TEST_PYTHON to require JSONL integration")
	}
	cmd := exec.Command(python, "-u", "tests/discovery_fixture.py")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }) }
	t.Cleanup(stop)
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		var result struct {
			Host string
			Port int
		}
		if json.Unmarshal([]byte(line), &result) != nil || result.Port == 0 {
			stop()
			t.Fatalf("Python fixture startup: %s %s", line, stderr.String())
		}
		d, err := newDirectoryClient(result.Host, result.Port, result.Port, false)
		if err != nil {
			t.Fatal(err)
		}
		d.networkTime = func(context.Context) (time.Time, error) { return time.Now(), nil }
		return d, stop
	case <-time.After(8 * time.Second):
		stop()
		t.Fatalf("Python fixture startup timeout: %s", stderr.String())
	}
	return nil, stop
}

func TestDirectoryTCPConfiguration(t *testing.T) {
	for _, host := range []string{"www.soulless.ai", "www.soulless.ai/v/api/ghost", "www.soulless.ai/v/api/ghost/", "127.0.0.1", "2001:db8::1", "[::1]"} {
		d, err := newDirectoryClient(host, 443, 443, true)
		if err != nil || !d.tls || d.port != 443 {
			t.Fatalf("configuration %s: %+v %v", host, d, err)
		}
		if strings.HasPrefix(host, "www.soulless.ai") && d.host != "www.soulless.ai" {
			t.Fatalf("address label was not normalized to the TLS hostname: %s", d.host)
		}
	}
	for _, host := range []string{"", "https://www.soulless.ai", "http://www.soulless.ai/v/api/ghost", "www.soulless.ai/other", "user@host", "bad\nname", "www.soulless.ai:443"} {
		if _, err := newDirectoryClient(host, 443, 443, true); err == nil {
			t.Fatalf("accepted URL/path or malformed hostname: %q", host)
		}
	}
	if _, err := newDirectoryClient("localhost", 443, 444, true); err == nil {
		t.Fatal("accepted split discovery ports")
	}
}

func TestDirectoryClientProtocol(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint("tls=", secure), func(t *testing.T) {
			d := startJSONLServer(t, secure, opaqueStore(10*time.Minute, nil))
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			id, err := d.publish(context.Background(), key, "http://203.0.113.7:8788")
			if err != nil {
				t.Fatal(err)
			}
			endpoint, pub, err := d.resolve(context.Background(), id)
			if err != nil || endpoint != "http://203.0.113.7:8788" || pub != base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) {
				t.Fatalf("signed set/get roundtrip: %s %s %v", endpoint, pub, err)
			}
			if directoryKey(id) != "ghost:v1:"+strconv.FormatInt(id, 10) {
				t.Fatal("unstable discovery key")
			}
		})
	}
}

func TestDirectoryTLSNeverSendsPlaintext(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	first := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		header := make([]byte, 5)
		_, _ = io.ReadFull(c, header)
		first <- header
	}()
	p := l.Addr().(*net.TCPAddr).Port
	d, _ := newDirectoryClient("127.0.0.1", p, p, true)
	if _, err := d.exchangeJSONLine(context.Background(), directoryRequest{Op: "get", Key: "test"}); err == nil {
		t.Fatal("non-TLS server accepted")
	}
	select {
	case header := <-first:
		if header[0] != 0x16 || header[1] != 0x03 {
			t.Fatalf("sent plaintext instead of TLS ClientHello: %x", header)
		}
	case <-time.After(time.Second):
		t.Fatal("no connection")
	}
	for _, e := range d.diagnostics.events {
		if e.Stage == "directory.json.write" {
			t.Fatal("JSON sent before TLS handshake succeeded")
		}
	}
}

func TestDirectoryTLSRejectsUntrustedCertificate(t *testing.T) {
	called := make(chan bool, 1)
	d := startJSONLServer(t, true, func(r directoryRequest) []byte { called <- true; return nil })
	d.tlsConfig = nil // Use the default trust store; the fixture CA is not trusted.
	_, err := d.exchangeJSONLine(context.Background(), directoryRequest{Op: "get", Key: "test"})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted certificate accepted: %v", err)
	}
	select {
	case <-called:
		t.Fatal("application data sent to an untrusted server")
	default:
	}
}

func TestDirectoryJSONLFailures(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	id := directoryID(key.Public().(ed25519.PublicKey))
	record, _ := packDirectoryRecord(key, "http://127.0.0.1:8788", uint32(time.Now().Unix()))
	valid := base64.RawURLEncoding.EncodeToString(record[:])
	cases := []struct {
		name, stage string
		reply       func(directoryRequest) []byte
	}{
		{"missing", "not found", func(r directoryRequest) []byte { return jsonLine(map[string]any{"ok": false, "error": "not found"}) }},
		{"expired", "expired", func(r directoryRequest) []byte { return jsonLine(map[string]any{"ok": false, "error": "expired"}) }},
		{"rate", "rate limit", func(r directoryRequest) []byte { return jsonLine(map[string]any{"ok": false, "error": "rate limit"}) }},
		{"bad-json", "directory.json.decode", func(r directoryRequest) []byte { return []byte("not json\n") }},
		{"http-listener", "returned HTTP 400 instead of JSONL", func(r directoryRequest) []byte { return []byte("HTTP/1.1 400 Bad Request\r\n\r\n") }},
		{"no-ok", "directory.json.decode", func(r directoryRequest) []byte { return []byte("{}\n") }},
		{"partial-line", "directory.json.read", func(r directoryRequest) []byte { return []byte("{\"ok\":true}") }},
		{"oversize", "4096-byte", func(r directoryRequest) []byte { return append(bytes.Repeat([]byte("x"), 4097), '\n') }},
		{"wrong-key", "reply key", func(r directoryRequest) []byte {
			return jsonLine(map[string]any{"ok": true, "key": "WRONG", "value": valid, "expires": futureExpiry()})
		}},
		{"missing-value", "missing the value", func(r directoryRequest) []byte {
			return jsonLine(map[string]any{"ok": true, "key": r.Key, "expires": futureExpiry()})
		}},
		{"opaque-garbage", "not a Ghost", func(r directoryRequest) []byte {
			return jsonLine(map[string]any{"ok": true, "key": r.Key, "value": "arbitrary opaque value", "expires": futureExpiry()})
		}},
		{"expired-lease", "directory.lease", func(r directoryRequest) []byte {
			return jsonLine(map[string]any{"ok": true, "key": r.Key, "value": valid, "expires": time.Now().Unix() - 1})
		}},
		{"missing-lease", "directory.lease", func(r directoryRequest) []byte {
			return jsonLine(map[string]any{"ok": true, "key": r.Key, "value": valid})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := startJSONLServer(t, false, tc.reply)
			_, _, err := d.resolve(context.Background(), id)
			if err == nil || !strings.Contains(err.Error(), tc.stage) {
				t.Fatalf("want %s: %v", tc.stage, err)
			}
			for _, e := range d.diagnostics.events {
				if e.Stage == "directory.verify" && e.Status == "ok" {
					t.Fatal("reported a verified advertisement on failure")
				}
			}
		})
	}
}

func TestDirectorySignedValuesAreVerifiedOnTheClient(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	id := directoryID(key.Public().(ed25519.PublicKey))
	for _, kind := range []string{"signature", "wrong-id", "old", "future"} {
		t.Run(kind, func(t *testing.T) {
			stamp := time.Now().Unix()
			if kind == "old" {
				stamp -= 1861
			}
			if kind == "future" {
				stamp += 301
			}
			record, _ := packDirectoryRecord(key, "http://127.0.0.1:8788", uint32(stamp))
			if kind == "signature" {
				record[127] ^= 1
			}
			targetID := id
			if kind == "wrong-id" {
				targetID ^= 1
			}
			d := startJSONLServer(t, false, func(r directoryRequest) []byte {
				return jsonLine(map[string]any{"ok": true, "key": r.Key, "value": base64.RawURLEncoding.EncodeToString(record[:]), "expires": futureExpiry()})
			})
			if _, _, err := d.resolve(context.Background(), targetID); err == nil {
				t.Fatal("accepted forged or stale signed peer value")
			}
		})
	}
}

func TestDirectoryCancellationDuringTLSAndRead(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint("tls=", secure), func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				c, err := l.Accept()
				if err == nil {
					accepted <- c
				}
			}()
			port := l.Addr().(*net.TCPAddr).Port
			d, _ := newDirectoryClient("127.0.0.1", port, port, secure)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := d.exchangeJSONLine(ctx, directoryRequest{Op: "get", Key: "test"}); done <- err }()
			select {
			case c := <-accepted:
				defer c.Close()
			case <-time.After(time.Second):
				t.Fatal("not connected")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled operation succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt I/O")
			}
		})
	}
}

func TestDiscoveryDNSFailureHasItsOwnStage(t *testing.T) {
	d, _ := newDirectoryClient("directory.invalid", 443, 443, true)
	d.dialer = &net.Dialer{Resolver: &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("simulated DNS failure")
	}}}
	_, err := d.exchangeJSONLine(context.Background(), directoryRequest{Op: "get", Key: "test"})
	if err == nil {
		t.Fatal("DNS failure succeeded")
	}
	for _, e := range d.diagnostics.events {
		if e.Stage == "directory.dns" && e.Status == "error" {
			return
		}
	}
	t.Fatal("DNS stage was not logged")
}

func TestDirectoryHeartbeatRefreshesBeforeLeaseExpires(t *testing.T) {
	sets := make(chan time.Time, 8)
	d := startJSONLServer(t, false, opaqueStore(600*time.Millisecond, func() { sets <- time.Now() }))
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	m := newMesh(h, key)
	defer m.close()
	m.startDirectory(d, "http://127.0.0.1:8788")
	var first time.Time
	select {
	case first = <-sets:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial set")
	}
	select {
	case second := <-sets:
		if second.Sub(first) >= 600*time.Millisecond {
			t.Fatal("heartbeat occurred after lease expired")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lease was not refreshed")
	}
}

func TestDiscoveryIdentityPinAndPeerFailure(t *testing.T) {
	for _, wrongIdentity := range []bool{false, true} {
		t.Run(fmt.Sprint("wrongIdentity=", wrongIdentity), func(t *testing.T) {
			h := newHub(filepath.Join(t.TempDir(), "hub.json"))
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			h.mesh = newMesh(h, key)
			_, advertisedKey, _ := ed25519.GenerateKey(rand.Reader)
			endpoint := "http://127.0.0.1:1"
			if wrongIdentity {
				other := newHub(filepath.Join(t.TempDir(), "other.json"))
				_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
				m := newMesh(other, otherKey)
				if err := m.start("127.0.0.1:0", "", "", "", false); err != nil {
					t.Fatal(err)
				}
				defer m.close()
				endpoint = m.url
			}
			record, _ := packDirectoryRecord(advertisedKey, endpoint, uint32(time.Now().Unix()))
			h.mesh.directory = startJSONLServer(t, false, func(r directoryRequest) []byte {
				return jsonLine(map[string]any{"ok": true, "key": r.Key, "value": base64.RawURLEncoding.EncodeToString(record[:]), "expires": futureExpiry()})
			})
			s := httptest.NewServer(h.routes())
			defer s.Close()
			alice := newPerson(t, s, "Alice")
			testAPI(t, s, "POST", "/api/peers/connect", alice, map[string]string{"address": fmt.Sprint(directoryID(advertisedKey.Public().(ed25519.PublicKey)))}, 400)
			if len(h.mesh.peers) != 0 {
				t.Fatal("failed handshake saved a peer")
			}
			want := "peer.hello"
			if wrongIdentity {
				want = "peer.identity"
			}
			found := false
			for _, e := range h.diagnostics.events {
				if e.Stage == want && e.Status == "error" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s error", want)
			}
		})
	}
}

func TestLiveDiscoveryTLSGet(t *testing.T) {
	if os.Getenv("GHOST_LIVE_DISCOVERY") != "1" {
		t.Skip("set GHOST_LIVE_DISCOVERY=1 to read-test the deployed TLS stream")
	}
	d, _ := newDirectoryClient("www.soulless.ai", 443, 443, true)
	suffix, err := randomID(26)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.exchangeJSONLine(context.Background(), directoryRequest{Op: "get", Key: "ghost:probe:" + suffix})
	if err != nil && !directoryMissing(err) {
		t.Fatal(err)
	}
	t.Log("Verified www.soulless.ai:443 TLS certificate/SNI and a JSONL get; no server records written")
}

func TestDirectorySetFailureDoesNotReportPublication(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for _, kind := range []string{"rejected", "wrong-key", "expired-lease"} {
		t.Run(kind, func(t *testing.T) {
			d := startJSONLServer(t, false, func(r directoryRequest) []byte {
				if kind == "rejected" {
					return jsonLine(map[string]any{"ok": false, "error": "capacity exceeded"})
				}
				echo, expires := r.Key, futureExpiry()
				if kind == "wrong-key" {
					echo = "different key"
				}
				if kind == "expired-lease" {
					expires = 0
				}
				return jsonLine(map[string]any{"ok": true, "key": echo, "expires": expires})
			})
			if id, err := d.publish(context.Background(), key, "http://127.0.0.1:8788"); err == nil || id != 0 {
				t.Fatalf("reported a publication despite %s: %d %v", kind, id, err)
			}
		})
	}
}

func TestDirectoryTestAPICleansUpItsProbe(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprint("publish=", publish), func(t *testing.T) {
			store := opaqueStore(10*time.Minute, nil)
			var mu sync.Mutex
			var operations []string
			d := startJSONLServer(t, false, func(r directoryRequest) []byte {
				mu.Lock()
				operations = append(operations, r.Op)
				mu.Unlock()
				return store(r)
			})
			h := newHub(filepath.Join(t.TempDir(), "hub.json"))
			h.settings = &settingsAPI{active: defaultNetworkSettings()}
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			h.mesh = newMesh(h, key)
			s := httptest.NewServer(h.routes())
			defer s.Close()
			alice := newPerson(t, s, "Alice")
			payload := map[string]any{"host": d.host, "publish": d.port, "tls": false}
			if publish {
				payload["advertise"] = "http://127.0.0.1:8788"
			}
			testAPI(t, s, "POST", "/api/settings/test-directory", testIdentity{}, payload, 401)
			testAPI(t, s, "POST", "/api/settings/test-directory", alice, payload, 200)
			mu.Lock()
			got := strings.Join(operations, ",")
			mu.Unlock()
			want := "get"
			if publish {
				want = "get,set,get,delete"
			}
			if got != want {
				t.Fatalf("expected only documented JSONL operations %s, got %s", want, got)
			}
		})
	}
}

func TestDirectoryTestKeepsTheLiveAdvertisement(t *testing.T) {
	d := startJSONLServer(t, false, opaqueStore(10*time.Minute, nil))
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	h.mesh = newMesh(h, key)
	h.mesh.url = "http://127.0.0.1:8788"
	cfg := defaultNetworkSettings()
	cfg.PeerAdvertise = h.mesh.url
	h.settings = &settingsAPI{active: cfg}
	id, err := d.publish(context.Background(), key, h.mesh.url)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h.routes())
	defer s.Close()
	alice := newPerson(t, s, "Alice")
	testAPI(t, s, "POST", "/api/settings/test-directory", alice, map[string]any{"host": d.host, "publish": d.port, "tls": false, "advertise": "http://127.0.0.1:9999"}, 200)
	endpoint, _, err := d.resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != h.mesh.url {
		t.Fatalf("testing unsaved settings replaced the live endpoint: %s", endpoint)
	}
}
