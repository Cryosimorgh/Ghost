package main

// Peer-to-peer rendezvous: every desktop process owns a node and talks directly
// to other nodes over the LAN or by an explicitly supplied address. No directory,
// relay, or other Ghost-operated server is involved.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const discoveryAddr = "239.192.67.71:47383"
const peerUserLimit = 32

type MeshUser struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	CallID string `json:"callId"`
}
type MeshHello struct {
	Version int        `json:"version"`
	Key     string     `json:"key"`
	Users   []MeshUser `json:"users"`
}
type MeshQueued struct {
	Path      string          `json:"path"`
	Body      json.RawMessage `json:"body"`
	Time      string          `json:"time"`
	Nonce     string          `json:"nonce"`
	Signature string          `json:"signature"`
}
type MeshPeer struct {
	URL                 string       `json:"url"`
	Key                 string       `json:"key"`
	LastSeen            time.Time    `json:"lastSeen"`
	LastPoll            time.Time    `json:"-"`
	LastDirectoryLookup time.Time    `json:"-"`
	Pending             []MeshQueued `json:"-"`
	Users               []MeshUser   `json:"users"`
}
type Mesh struct {
	h               *Hub
	private         ed25519.PrivateKey
	public          string
	listener        net.Listener
	http            *http.Server
	url             string
	port            int
	mu              sync.RWMutex
	peers           map[string]*MeshPeer // keyed by peer public key
	nonces          map[string]time.Time
	acks            map[string]map[string]bool
	client          *http.Client
	directory       *directoryClient
	directoryCode   int64
	directoryStatus string
	directoryError  string
	stop            chan struct{}
	directMu        sync.Mutex
	direct          map[string]*directSession
	invite          *directPendingInvite
}

func meshKey(path string) (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		d, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(d) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("invalid mesh key in %s", path)
		}
		return ed25519.PrivateKey(d), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	// Refuse to overwrite a competing process's identity.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return meshKey(path)
	}
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(private) + "\n")
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return nil, err
	}
	return private, nil
}
func newMesh(h *Hub, private ed25519.PrivateKey) *Mesh {
	key := base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey))
	return &Mesh{h: h, private: private, public: key, peers: make(map[string]*MeshPeer), nonces: make(map[string]time.Time), acks: make(map[string]map[string]bool), stop: make(chan struct{}), direct: make(map[string]*directSession), client: &http.Client{Transport: networkHTTPTransport(), Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (m *Mesh) activeUsers() []MeshUser {
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	out := make([]MeshUser, 0)
	for _, s := range m.h.sessions {
		u := m.h.state.Users[s.UserID]
		if u == nil || u.RemoteKey != "" {
			continue
		}
		out = append(out, MeshUser{ID: u.ID, Name: u.Name, CallID: s.CallID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID+out[i].CallID < out[j].ID+out[j].CallID })
	if len(out) > peerUserLimit {
		out = out[:peerUserLimit]
	}
	return out
}
func safePeerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("peer address is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return "", errors.New("use an http(s) host:port address without a path")
	}
	if p := u.Port(); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 || v > 65535 {
			return "", errors.New("invalid port")
		}
	}
	return u.Scheme + "://" + u.Host, nil
}
func (m *Mesh) signature(method, path string, body []byte) (ts, nonce, sig string, err error) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return
	}
	nonce = hex.EncodeToString(b)
	sig = base64.RawURLEncoding.EncodeToString(ed25519.Sign(m.private, []byte(signatureInput(method, path, ts, nonce, body))))
	return
}
func signatureInput(method, path, ts, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return method + "\n" + path + "\n" + ts + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
}
func (m *Mesh) authenticatePeer(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "oversize request", 413)
		return "", nil, false
	}
	key := r.Header.Get("X-Ghost-Node-Key")
	pk, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(pk) != ed25519.PublicKeySize {
		http.Error(w, "bad node key", 401)
		return "", nil, false
	}
	ts := r.Header.Get("X-Ghost-Time")
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(t, 0)) > 5*time.Minute || time.Until(time.Unix(t, 0)) > 5*time.Minute {
		http.Error(w, "stale message", 401)
		return "", nil, false
	}
	nonce := r.Header.Get("X-Ghost-Nonce")
	if len(nonce) != 32 {
		http.Error(w, "invalid nonce", 401)
		return "", nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Ghost-Signature"))
	if err != nil || !ed25519.Verify(pk, []byte(signatureInput(r.Method, r.URL.Path, ts, nonce, data)), sig) {
		http.Error(w, "bad signature", 401)
		return "", nil, false
	}
	m.mu.Lock()
	now := time.Now()
	for k, v := range m.nonces {
		if now.Sub(v) > 6*time.Minute {
			delete(m.nonces, k)
		}
	}
	keyNonce := key + ":" + nonce
	if _, ok := m.nonces[keyNonce]; ok {
		m.mu.Unlock()
		http.Error(w, "replayed message", 409)
		return "", nil, false
	}
	m.nonces[keyNonce] = now
	m.mu.Unlock()
	return key, data, true
}
func (m *Mesh) post(ctx context.Context, peer *MeshPeer, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if active, err := m.directPost(ctx, peer.Key, path, b); active {
		return err
	}
	// A non-public peer can make a signed outbound poll to a reachable peer.
	// Its replies travel back over that same request, no inbound port needed.
	if m.queuePolled(peer.Key, path, b) {
		return nil
	}
	ts, nonce, sig, err := m.signature("POST", path, b)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", peer.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ghost-Node-Key", m.public)
	req.Header.Set("X-Ghost-Time", ts)
	req.Header.Set("X-Ghost-Nonce", nonce)
	req.Header.Set("X-Ghost-Signature", sig)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		txt, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("peer rejected %s (%d): %s", path, resp.StatusCode, strings.TrimSpace(string(txt)))
	}
	return nil
}
func (m *Mesh) getHello(ctx context.Context, raw string) (MeshHello, error) {
	addr, err := safePeerURL(raw)
	if err != nil {
		return MeshHello{}, err
	}
	challengeBytes := make([]byte, 16)
	if _, err = rand.Read(challengeBytes); err != nil {
		return MeshHello{}, err
	}
	challenge := hex.EncodeToString(challengeBytes)
	req, err := http.NewRequestWithContext(ctx, "GET", addr+"/p2p/hello", nil)
	if err != nil {
		return MeshHello{}, err
	}
	req.Header.Set("X-Ghost-Challenge", challenge)
	res, err := m.client.Do(req)
	if err != nil {
		return MeshHello{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return MeshHello{}, fmt.Errorf("peer returned %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 16384))
	if err != nil {
		return MeshHello{}, err
	}
	var hello MeshHello
	if err = json.Unmarshal(b, &hello); err != nil {
		return MeshHello{}, err
	}
	pk, err := base64.RawURLEncoding.DecodeString(hello.Key)
	if hello.Version != 1 || err != nil || len(pk) != ed25519.PublicKeySize || len(hello.Users) > peerUserLimit {
		return MeshHello{}, errors.New("incompatible or invalid Ghost peer")
	}
	sig, err := base64.RawURLEncoding.DecodeString(res.Header.Get("X-Ghost-Hello-Signature"))
	if err != nil || !ed25519.Verify(pk, []byte("hello-response\n"+challenge+"\n"+hex.EncodeToString(sha256sum(b))), sig) {
		return MeshHello{}, errors.New("peer identity signature failed")
	}
	for _, u := range hello.Users {
		if len(u.ID) != 10 || len(u.CallID) != 9 || len(u.Name) > 128 {
			return MeshHello{}, errors.New("invalid peer profile")
		}
	}
	return hello, nil
}
func (m *Mesh) peer(key string) *MeshPeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.peers[key]
	if p == nil {
		return nil
	}
	copy := *p
	copy.Users = append([]MeshUser(nil), p.Users...)
	return &copy
}
func (m *Mesh) findUser(id string) (*MeshPeer, MeshUser) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.peers {
		if time.Since(p.LastSeen) > 30*time.Second {
			continue
		}
		for _, u := range p.Users {
			if u.ID == id {
				copy := *p
				return &copy, u
			}
		}
	}
	return nil, MeshUser{}
}
func (m *Mesh) findCall(call string) *MeshPeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.peers {
		if time.Since(p.LastSeen) > 30*time.Second {
			continue
		}
		for _, u := range p.Users {
			if u.CallID == call {
				copy := *p
				return &copy
			}
		}
	}
	return nil
}
func (m *Mesh) addPeer(url string, hello MeshHello) error {
	if hello.Key == m.public {
		return errors.New("this is your own Ghost node")
	}
	m.mu.Lock()
	p := m.peers[hello.Key]
	if p == nil {
		p = &MeshPeer{Key: hello.Key}
		m.peers[hello.Key] = p
	}
	// An existing node identity must not silently change its advertised route
	// when added manually. LAN discovery does legitimately update IP addresses.
	oldUsers := append([]MeshUser(nil), p.Users...)
	p.URL = url
	p.LastSeen = time.Now()
	p.Users = hello.Users
	m.mu.Unlock()
	m.h.mu.Lock()
	for _, u := range append(oldUsers, hello.Users...) {
		m.h.notifyFriendsLocked(u.ID)
	}
	m.h.mu.Unlock()
	return nil
}
func (m *Mesh) connect(ctx context.Context, raw string, introduce bool) error {
	return m.connectPinned(ctx, raw, "", introduce)
}
func (m *Mesh) connectPinned(ctx context.Context, raw, expectedKey string, introduce bool) error {
	ctx, trace := m.h.diagnostics.begin(ctx, "peer.connect")
	normalized, err := safePeerURL(raw)
	if err != nil {
		return trace.fail("peer.address", err)
	}
	trace.event("peer.hello", "start", "GET "+normalized+"/p2p/hello; requesting signed identity challenge")
	hello, err := m.getHello(ctx, normalized)
	if err != nil {
		return trace.fail("peer.hello", err)
	}
	if expectedKey != "" && hello.Key != expectedKey {
		return trace.fail("peer.identity", errors.New("peer key does not match signed directory advertisement"))
	}
	trace.event("peer.identity", "ok", "Signed peer identity verified")
	if err = m.addPeer(normalized, hello); err != nil {
		return trace.fail("peer.register", err)
	}
	if introduce {
		trace.event("peer.poll", "start", "Establishing signed bidirectional signaling")
		if err = m.pollOne(ctx, normalized, hello.Key); err != nil {
			return trace.fail("peer.poll", err)
		}
		m.persistPeer(hello.Key, normalized)
	}
	trace.event("peer.connect", "ok", "Peer handshake and signaling established at "+normalized)
	return nil
}
func (m *Mesh) meshRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/p2p/poll", m.handlePoll)
	mux.HandleFunc("/p2p/hello", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "GET") {
			return
		}
		challenge := r.Header.Get("X-Ghost-Challenge")
		if len(challenge) != 32 {
			http.Error(w, "challenge required", 400)
			return
		}
		if _, err := hex.DecodeString(challenge); err != nil {
			http.Error(w, "invalid challenge", 400)
			return
		}
		body, _ := json.Marshal(MeshHello{Version: 1, Key: m.public, Users: m.activeUsers()})
		sig := ed25519.Sign(m.private, []byte("hello-response\n"+challenge+"\n"+hex.EncodeToString(sha256sum(body))))
		w.Header().Set("X-Ghost-Hello-Signature", base64.RawURLEncoding.EncodeToString(sig))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/p2p/friend/request", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "POST") {
			return
		}
		key, b, ok := m.authenticatePeer(w, r)
		if !ok {
			return
		}
		var q struct {
			From MeshUser `json:"from"`
			To   string   `json:"to"`
		}
		if json.Unmarshal(b, &q) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		p := m.peer(key)
		if p == nil || !meshHasUser(p.Users, q.From.ID, q.From.CallID) {
			http.Error(w, "unknown calling peer", 403)
			return
		}
		m.h.mu.Lock()
		defer m.h.mu.Unlock()
		to := m.h.state.Users[q.To]
		if to == nil || to.RemoteKey != "" {
			http.Error(w, "unknown local recipient", 404)
			return
		}
		if q.From.ID == to.ID {
			http.Error(w, "same account", 400)
			return
		}
		other := m.h.state.Users[q.From.ID]
		if other != nil && (other.RemoteKey == "" || other.RemoteKey != key) {
			http.Error(w, "identity collision", 409)
			return
		}
		if other == nil {
			other = &User{ID: q.From.ID, Name: q.From.Name, RemoteKey: key}
			m.h.state.Users[q.From.ID] = other
		}
		if !contains(to.Friends, q.From.ID) && !contains(to.Incoming, q.From.ID) {
			if len(to.Incoming) >= 100 {
				http.Error(w, "inbox full", 429)
				return
			}
			to.Incoming = append(to.Incoming, q.From.ID)
			if err := m.h.saveLocked(); err != nil {
				http.Error(w, "storage failed", 500)
				return
			}
			m.h.emitLocked(to.ID, Event{Type: "inbox"})
		}
		sendJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/p2p/friend/respond", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "POST") {
			return
		}
		key, b, ok := m.authenticatePeer(w, r)
		if !ok {
			return
		}
		var q struct {
			From   string `json:"from"`
			To     string `json:"to"`
			Accept bool   `json:"accept"`
		}
		if json.Unmarshal(b, &q) != nil {
			http.Error(w, "bad response", 400)
			return
		}
		p := m.peer(key)
		if p == nil || !meshHasID(p.Users, q.From) {
			http.Error(w, "unknown sender", 403)
			return
		}
		m.h.mu.Lock()
		defer m.h.mu.Unlock()
		to := m.h.state.Users[q.To]
		remote := m.h.state.Users[q.From]
		if to == nil || to.RemoteKey != "" || remote == nil || remote.RemoteKey != key || !contains(to.Outgoing, q.From) {
			http.Error(w, "invitation not found", 404)
			return
		}
		to.Outgoing = remove(to.Outgoing, q.From)
		if q.Accept && !contains(to.Friends, q.From) {
			to.Friends = append(to.Friends, q.From)
		}
		if err := m.h.saveLocked(); err != nil {
			http.Error(w, "storage failed", 500)
			return
		}
		m.h.emitLocked(to.ID, Event{Type: "inbox"})
		sendJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/p2p/signal", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "POST") {
			return
		}
		key, b, ok := m.authenticatePeer(w, r)
		if !ok {
			return
		}
		var q struct {
			From     string          `json:"from"`
			FromCall string          `json:"fromCall"`
			To       string          `json:"to"`
			Kind     string          `json:"kind"`
			Data     json.RawMessage `json:"data"`
		}
		if json.Unmarshal(b, &q) != nil || !validSignal(q.Kind, q.Data) {
			http.Error(w, "bad signal", 400)
			return
		}
		p := m.peer(key)
		if p == nil || !meshHasUser(p.Users, q.From, q.FromCall) {
			http.Error(w, "unknown signaling source", 403)
			return
		}
		m.h.mu.Lock()
		defer m.h.mu.Unlock()
		to := m.h.calls[q.To]
		if to == nil {
			http.Error(w, "call ID offline", 404)
			return
		}
		user := m.h.state.Users[q.From]
		name := "Peer"
		if user != nil && user.RemoteKey == key {
			name = user.Name
		} else {
			for _, u := range p.Users {
				if u.ID == q.From {
					name = u.Name
					break
				}
			}
		}
		select {
		case to.Events <- Event{Type: "signal", Data: map[string]any{"fromCallId": q.FromCall, "fromUserId": q.From, "fromName": name, "kind": q.Kind, "data": q.Data}}:
			sendJSON(w, 200, map[string]bool{"ok": true})
		default:
			http.Error(w, "recipient busy", 429)
		}
	})
	return mux
}
func meshHasUser(users []MeshUser, id, call string) bool {
	for _, u := range users {
		if u.ID == id && u.CallID == call {
			return true
		}
	}
	return false
}
func meshHasID(users []MeshUser, id string) bool {
	for _, u := range users {
		if u.ID == id {
			return true
		}
	}
	return false
}
func validSignal(kind string, data []byte) bool {
	allowed := map[string]bool{"invite": true, "accept": true, "decline": true, "offer": true, "answer": true, "ice": true, "hangup": true}
	return allowed[kind] && len(data) < 60000
}
func (m *Mesh) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	if m.h.authenticate(w, r) == nil {
		return
	}
	type view struct {
		URL    string     `json:"url"`
		Online bool       `json:"online"`
		Users  []MeshUser `json:"users"`
		Key    string     `json:"key"`
	}
	m.mu.RLock()
	out := make([]view, 0, len(m.peers))
	for _, p := range m.peers {
		online := time.Since(p.LastSeen) < 30*time.Second
		users := p.Users
		if !online {
			users = nil
		}
		out = append(out, view{URL: p.URL, Online: online, Users: users, Key: p.Key})
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	m.mu.RLock()
	code, enabled := m.directoryCode, m.directory != nil
	directoryStatus, directoryError := m.directoryStatus, m.directoryError
	m.mu.RUnlock()
	m.directMu.Lock()
	links := len(m.direct)
	inviteOpen := m.invite != nil
	m.directMu.Unlock()
	// 63-bit IDs exceed JavaScript Number precision: always transmit as decimal text.
	codeText := ""
	if code > 0 {
		codeText = strconv.FormatInt(code, 10)
	}
	sendJSON(w, 200, map[string]any{"directLinks": links, "directInviteOpen": inviteOpen, "peers": out, "nodeKey": m.public, "address": m.url, "addresses": m.addresses(), "directoryCode": codeText, "directoryEnabled": enabled, "directoryStatus": directoryStatus, "directoryError": directoryError, "discovery": "LAN multicast + optional short-lived rendezvous directory"})
}
func (m *Mesh) apiConnect(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	if m.h.authenticate(w, r) == nil {
		return
	}
	var q struct {
		Address string `json:"address"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	ctx, trace := m.h.diagnostics.begin(r.Context(), "discovery.connect")
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	address := strings.TrimSpace(q.Address)
	expected := ""
	if len(address) <= 19 && len(address) > 0 && strings.IndexFunc(address, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		id, err := strconv.ParseInt(address, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid directory code", 400)
			return
		}
		m.mu.RLock()
		d := m.directory
		m.mu.RUnlock()
		if d == nil {
			http.Error(w, trace.fail("directory.config", errors.New("configure the discovery server in Settings to resolve codes")).Error(), 400)
			return
		}
		var errResolve error
		address, expected, errResolve = d.resolve(ctx, id)
		if errResolve != nil {
			http.Error(w, errResolve.Error(), 400)
			return
		}
	}
	if err := m.connectPinned(ctx, address, expected, true); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	sendJSON(w, 200, map[string]any{"ok": true, "attempt": trace.id})
}
func (m *Mesh) sendSignal(ctx context.Context, peer *MeshPeer, from *Session, name, to, kind string, data json.RawMessage) error {
	ctx, trace := m.h.diagnostics.begin(ctx, "peer.signal")
	trace.event("peer.signal", "start", "Sending signaling event (payload omitted)")
	if err := m.post(ctx, peer, "/p2p/signal", map[string]any{"from": from.UserID, "fromCall": from.CallID, "to": to, "kind": kind, "data": data}); err != nil {
		return trace.fail("peer.signal", err)
	}
	trace.event("peer.signal", "ok", "Signaling event delivered or queued for outbound polling")
	return nil
}
func (m *Mesh) refresh(ctx context.Context) {
	m.mu.RLock()
	list := make([]MeshPeer, 0, len(m.peers))
	d := m.directory
	for _, p := range m.peers {
		list = append(list, *p)
	}
	m.mu.RUnlock()
	for _, p := range list {
		m.directMu.Lock()
		active := m.direct[p.Key] != nil
		m.directMu.Unlock()
		if active {
			continue
		}
		reachable := false
		if p.URL != "" && !strings.HasPrefix(p.URL, "direct://") {
			ctx2, cancel := context.WithTimeout(ctx, 4*time.Second)
			hello, err := m.getHello(ctx2, p.URL)
			cancel()
			if err == nil && hello.Key == p.Key {
				_ = m.addPeer(p.URL, hello)
				reachable = true
			}
		}
		if reachable || d == nil || time.Since(p.LastDirectoryLookup) < time.Minute {
			continue
		}
		m.mu.Lock()
		if current := m.peers[p.Key]; current != nil {
			current.LastDirectoryLookup = time.Now()
		}
		m.mu.Unlock()
		pub, err := base64.RawURLEncoding.DecodeString(p.Key)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		ctx2, cancel := context.WithTimeout(ctx, 6*time.Second)
		address, key, err := d.resolve(ctx2, directoryID(ed25519.PublicKey(pub)))
		if err == nil && key == p.Key {
			// Keep the peer identity pinned even when its address changes.
			if err = m.connectPinned(ctx2, address, key, true); err == nil {
				m.persistPeer(key, address)
			}
		}
		cancel()
	}
}
func (m *Mesh) start(addr, advertise, cert, key string, discover bool) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	m.listener = l
	m.port = l.Addr().(*net.TCPAddr).Port
	scheme := "http"
	if cert != "" {
		scheme = "https"
	}
	if advertise != "" {
		m.url, err = safePeerURL(advertise)
		if err != nil {
			_ = l.Close()
			return err
		}
		if !strings.HasPrefix(m.url, scheme+"://") {
			_ = l.Close()
			return fmt.Errorf("peer address protocol must match configured TLS (expected %s)", scheme)
		}
	} else {
		m.url = fmt.Sprintf("%s://127.0.0.1:%d", scheme, m.port)
	}
	m.http = &http.Server{Handler: m.meshRoutes(), ReadHeaderTimeout: 8 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		var e error
		if cert != "" {
			e = m.http.ServeTLS(l, cert, key)
		} else {
			e = m.http.Serve(l)
		}
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			log.Printf("Ghost peer listener: %v", e)
		}
	}()
	go func() {
		ticker := time.NewTicker(6 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				m.refresh(ctx)
				cancel()
			case <-m.stop:
				return
			}
		}
	}()
	go m.pollLoop()
	if discover {
		go m.listenLAN()
		go m.announceLAN()
	}
	return nil
}
func (m *Mesh) close() {
	m.client.CloseIdleConnections()
	m.closeDirect()
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	if m.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.http.Shutdown(ctx)
	}
}
func (m *Mesh) listenLAN() {
	addr, _ := net.ResolveUDPAddr("udp4", discoveryAddr)
	c, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		log.Printf("LAN discovery receive disabled: %v", err)
		return
	}
	defer c.Close()
	_ = c.SetReadBuffer(65536)
	buf := make([]byte, 1024)
	for {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.stop:
				return
			default:
				continue
			}
		}
		var advert struct {
			Key     string `json:"key"`
			Port    int    `json:"port"`
			Version int    `json:"version"`
		}
		if json.Unmarshal(buf[:n], &advert) != nil || advert.Version != 1 || advert.Key == m.public || advert.Port < 1 || advert.Port > 65535 || src.IP.IsLoopback() || src.IP.IsUnspecified() {
			continue
		}
		m.mu.RLock()
		p := m.peers[advert.Key]
		fresh := p != nil && time.Since(p.LastSeen) < 4*time.Second
		m.mu.RUnlock()
		if fresh {
			continue
		}
		raw := fmt.Sprintf("http://%s", net.JoinHostPort(src.IP.String(), strconv.Itoa(advert.Port)))
		// LAN-only auto-connect. Remote hosts on the public internet never trigger it.
		if !src.IP.IsPrivate() && !src.IP.IsLinkLocalUnicast() {
			continue
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			_ = m.connect(ctx, raw, true)
		}()
	}
}
func (m *Mesh) announceLAN() {
	group, _ := net.ResolveUDPAddr("udp4", discoveryAddr)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	send := func() {
		c, err := net.DialUDP("udp4", nil, group)
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := json.Marshal(map[string]any{"version": 1, "port": m.port, "key": m.public})
		_, _ = c.Write(b)
	}
	send()
	for {
		select {
		case <-ticker.C:
			send()
		case <-m.stop:
			return
		}
	}
}

func (m *Mesh) localCall(id string) string {
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	call, _ := m.h.onlineLocked(id)
	return call
}

// On a LAN, announce the actual interface used to reach the other peer,
// not loopback. Internet peers require explicit -peer-advertise with a
// reachable forwarded port; this function does not punch through NAT.
func (m *Mesh) returnAddress(peerURL string) string {
	if !strings.Contains(m.url, "127.0.0.1") {
		return m.url
	}
	u, err := url.Parse(peerURL)
	if err != nil {
		return m.url
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "localhost" {
		return m.url
	}
	d, err := net.DialTimeout("udp", net.JoinHostPort(host, "9"), time.Second)
	if err != nil {
		return m.url
	}
	ip := d.LocalAddr().(*net.UDPAddr).IP.String()
	_ = d.Close()
	if ip == "" || ip == "0.0.0.0" {
		return m.url
	}
	scheme := "http"
	if strings.HasPrefix(m.url, "https://") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(ip, strconv.Itoa(m.port)))
}

// Pinned peer identities + addresses survive restarts. They are local data;
// nobody is asked to register with a centralized directory.
func (m *Mesh) persistPeer(key, addr string) {
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	if m.h.state.Peers == nil {
		m.h.state.Peers = make(map[string]string)
	}
	if m.h.state.Peers[key] == addr {
		return
	}
	m.h.state.Peers[key] = addr
	if err := m.h.saveLocked(); err != nil {
		log.Printf("saving peer address: %v", err)
	}
}
func (m *Mesh) restorePeers() {
	m.h.mu.Lock()
	saved := make(map[string]string, len(m.h.state.Peers))
	for k, v := range m.h.state.Peers {
		saved[k] = v
	}
	m.h.mu.Unlock()
	for key, addr := range saved {
		select {
		case <-m.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		hello, err := m.getHello(ctx, addr)
		if err == nil && hello.Key == key {
			_ = m.addPeer(addr, hello)
			if m.url != "" {
				_ = m.pollOne(ctx, addr, key)
			}
		}
		cancel()
	}
}

func (m *Mesh) addresses() []string {
	if m.listener == nil {
		return nil
	}
	scheme := "http"
	if strings.HasPrefix(m.url, "https://") {
		scheme = "https"
	}
	found := make(map[string]bool)
	out := []string{}
	add := func(s string) {
		if s != "" && !found[s] {
			out = append(out, s)
			found[s] = true
		}
	}
	add(m.url)
	interfaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ips, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range ips {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip.To4() == nil || !ip.IsPrivate() {
				continue
			}
			add(fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(ip.String(), strconv.Itoa(m.port))))
		}
	}
	sort.Strings(out)
	return out
}

// Polling is an outbound-only path used when one node is behind a firewall or
// CGNAT. The peer that receives the poll can send signaling and mailbox events
// back on its signed response. It does not need to dial the private node.
func (m *Mesh) queuePolled(key, path string, body []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.peers[key]
	if p == nil || time.Since(p.LastPoll) > 12*time.Second || len(p.Pending) >= 80 {
		return false
	}
	ts, nonce, sig, err := m.signature("POST", path, body)
	if err != nil {
		return false
	}
	p.Pending = append(p.Pending, MeshQueued{Path: path, Body: append([]byte(nil), body...), Time: ts, Nonce: nonce, Signature: sig})
	return true
}
func (m *Mesh) handlePoll(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	key, body, ok := m.authenticatePeer(w, r)
	if !ok {
		return
	}
	var q struct {
		Users []MeshUser `json:"users"`
		Acks  []string   `json:"acks"`
	}
	if json.Unmarshal(body, &q) != nil || len(q.Users) > peerUserLimit {
		http.Error(w, "invalid peer profiles", 400)
		return
	}
	for _, u := range q.Users {
		if len(u.ID) != 10 || len(u.CallID) != 9 || len(u.Name) > 128 {
			http.Error(w, "invalid user profile", 400)
			return
		}
	}
	m.mu.Lock()
	p := m.peers[key]
	if p == nil {
		p = &MeshPeer{Key: key}
		m.peers[key] = p
	}
	oldUsers := append([]MeshUser(nil), p.Users...)
	p.Users = q.Users
	p.LastSeen = time.Now()
	p.LastPoll = time.Now()
	if len(q.Acks) > 0 {
		ack := make(map[string]bool, len(q.Acks))
		for _, id := range q.Acks {
			ack[id] = true
		}
		remaining := p.Pending[:0]
		for _, event := range p.Pending {
			if !ack[event.Nonce] {
				remaining = append(remaining, event)
			}
		}
		p.Pending = remaining
	}
	// Pending messages remain queued until the receiver acknowledges them.
	// Failed responses cannot silently discard a friend reply or call offer.
	batch := p.Pending
	if len(batch) > 16 {
		batch = batch[:16]
	}
	events := append([]MeshQueued(nil), batch...)
	m.mu.Unlock()
	m.h.mu.Lock()
	for _, u := range append(oldUsers, q.Users...) {
		m.h.notifyFriendsLocked(u.ID)
	}
	m.h.mu.Unlock()
	encoded, _ := json.Marshal(map[string]any{"events": events})
	// Bind the signed response to the client's unique request nonce.
	sig := ed25519.Sign(m.private, []byte("poll-response\n"+r.Header.Get("X-Ghost-Nonce")+"\n"+hex.EncodeToString(sha256sum(encoded))))
	w.Header().Set("X-Ghost-Poll-Key", m.public)
	w.Header().Set("X-Ghost-Poll-Signature", base64.RawURLEncoding.EncodeToString(sig))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(encoded)
}
func sha256sum(b []byte) []byte { d := sha256.Sum256(b); return d[:] }
func (m *Mesh) pollOne(ctx context.Context, addr, key string) error {
	m.mu.RLock()
	pendingAcks := make([]string, 0)
	for id := range m.acks[key] {
		pendingAcks = append(pendingAcks, id)
	}
	m.mu.RUnlock()
	body, err := json.Marshal(map[string]any{"users": m.activeUsers(), "acks": pendingAcks})
	if err != nil {
		return err
	}
	ts, nonce, sig, err := m.signature("POST", "/p2p/poll", body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", addr+"/p2p/poll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Ghost-Node-Key", m.public)
	req.Header.Set("X-Ghost-Time", ts)
	req.Header.Set("X-Ghost-Nonce", nonce)
	req.Header.Set("X-Ghost-Signature", sig)
	response, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("peer poll failed: %d", response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, 1500000))
	if err != nil {
		return err
	}
	if response.Header.Get("X-Ghost-Poll-Key") != key {
		return errors.New("poll identity mismatch")
	}
	signature, err := base64.RawURLEncoding.DecodeString(response.Header.Get("X-Ghost-Poll-Signature"))
	if err != nil {
		return err
	}
	public, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || !ed25519.Verify(public, []byte("poll-response\n"+nonce+"\n"+hex.EncodeToString(sha256sum(b))), signature) {
		return errors.New("poll response signature failure")
	}
	var data struct {
		Events []MeshQueued `json:"events"`
	}
	if err = json.Unmarshal(b, &data); err != nil {
		return err
	}
	if len(data.Events) > 16 {
		return errors.New("too many polled events")
	}
	m.mu.Lock()
	for _, id := range pendingAcks {
		delete(m.acks[key], id)
	}
	m.mu.Unlock()
	for _, event := range data.Events {
		// Dispatch through the same authenticated HTTP handlers used by directly
		// reachable nodes. The original sender's signature is verified again.
		if !strings.HasPrefix(event.Path, "/p2p/") || event.Path == "/p2p/poll" {
			continue
		}
		r := httptest.NewRequest("POST", event.Path, bytes.NewReader(event.Body))
		r.Header.Set("X-Ghost-Node-Key", key)
		r.Header.Set("X-Ghost-Time", event.Time)
		r.Header.Set("X-Ghost-Nonce", event.Nonce)
		r.Header.Set("X-Ghost-Signature", event.Signature)
		result := httptest.NewRecorder()
		m.meshRoutes().ServeHTTP(result, r)
		if result.Code == 200 || result.Code == 409 {
			m.mu.Lock()
			if m.acks[key] == nil {
				m.acks[key] = make(map[string]bool)
			}
			m.acks[key][event.Nonce] = true
			m.mu.Unlock()
		} else {
			log.Printf("received peer event %s rejected: %d: %.120s", event.Path, result.Code, result.Body.String())
		}
	}
	return nil
}
func (m *Mesh) pollLoop() {
	ticker := time.NewTicker(2300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.RLock()
			ps := make([]MeshPeer, 0, len(m.peers))
			for _, p := range m.peers {
				if p.URL != "" {
					ps = append(ps, *p)
				}
			}
			m.mu.RUnlock()
			for _, p := range ps {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				if err := m.pollOne(ctx, p.URL, p.Key); err != nil {
					_, trace := m.h.diagnostics.begin(ctx, "peer.poll.retry")
					_ = trace.fail("peer.poll.retry", err)
				}
				cancel()
			}
		case <-m.stop:
			return
		}
	}
}
