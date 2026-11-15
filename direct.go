package main

// One-time, direct, authenticated TCP introduction and full-duplex signaling.
// This is an OPTIONAL funnel. Existing LAN, directory and persistent HTTP peer
// routes are untouched. A listener is closed after one successful pairing;
// the accepted socket remains open as an encrypted bidirectional tunnel.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const directPrefix = "GHOST1."
const directMagic = "GHOSTD1\n"
const directTTL = 10 * time.Minute
const directMaxFrame = 96 << 10

type directInvite struct {
	Version int    `json:"v"`
	Address string `json:"a"`
	Key     string `json:"k"`
	Secret  string `json:"s"`
	Expires int64  `json:"e"`
}

type directPendingInvite struct {
	listener net.Listener
	secret   [32]byte
	expires  time.Time
	code     string
	claimed  atomic.Bool
}

type directFrame struct {
	Type      string          `json:"t"`
	ID        uint64          `json:"i,omitempty"`
	Key       string          `json:"k,omitempty"`
	Users     []MeshUser      `json:"u,omitempty"`
	Path      string          `json:"p,omitempty"`
	Body      json.RawMessage `json:"b,omitempty"`
	Time      string          `json:"ts,omitempty"`
	Nonce     string          `json:"n,omitempty"`
	Signature string          `json:"sg,omitempty"`
	Status    int             `json:"st,omitempty"`
	Message   string          `json:"m,omitempty"`
}

type directCipher struct {
	c             net.Conn
	enc, dec      cipher.AEAD
	outSeq, inSeq uint64
	writeMu       sync.Mutex
}

type directSession struct {
	mesh       *Mesh
	wire       *directCipher
	key        string
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	pending    map[uint64]chan directFrame
	nextID     atomic.Uint64
	serveSlots chan struct{}
}

func directRandom(n int) ([]byte, error) {
	b := make([]byte, n)
	_, e := rand.Read(b)
	return b, e
}
func directMAC(key []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		_, _ = h.Write(p)
	}
	return h.Sum(nil)
}

// Invite is an opaque, authenticated bearer code. The symmetric decryption
// key is contained in the code so it can work offline without a lookup service:
// it conceals the address from casual inspection, NOT from code recipients.
func sealDirectInvite(q directInvite) (string, error) {
	key, e := directRandom(32)
	if e != nil {
		return "", e
	}
	nonce, e := directRandom(12)
	if e != nil {
		return "", e
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	raw, e := json.Marshal(q)
	if e != nil {
		return "", e
	}
	cipherText := aead.Seal(nil, nonce, raw, []byte(directPrefix))
	data := append(append(append([]byte(nil), key...), nonce...), cipherText...)
	return directPrefix + base64.RawURLEncoding.EncodeToString(data), nil
}
func openDirectInvite(code string, now time.Time) (directInvite, error) {
	var q directInvite
	if !strings.HasPrefix(code, directPrefix) || len(code) > 1100 {
		return q, errors.New("invalid direct invite")
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, directPrefix))
	if e != nil || len(b) < 32+12+16 {
		return q, errors.New("invalid direct invite")
	}
	block, e := aes.NewCipher(b[:32])
	if e != nil {
		return q, e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return q, e
	}
	clear, e := aead.Open(nil, b[32:44], b[44:], []byte(directPrefix))
	if e != nil {
		return q, errors.New("direct invite failed authentication")
	}
	if e := json.Unmarshal(clear, &q); e != nil {
		return q, errors.New("invalid direct invite")
	}
	sec, e := base64.RawURLEncoding.DecodeString(q.Secret)
	pk, ek := base64.RawURLEncoding.DecodeString(q.Key)
	if q.Version != 1 || e != nil || len(sec) != 32 || ek != nil || len(pk) != ed25519.PublicKeySize || now.Unix() >= q.Expires || q.Expires > now.Add(directTTL+time.Minute).Unix() {
		return q, errors.New("expired or invalid direct invite")
	}
	endpoint, e := url.Parse(q.Address)
	if e != nil || endpoint.Scheme != "tcp" || endpoint.Hostname() == "" || endpoint.Port() == "" || endpoint.Path != "" || endpoint.User != nil {
		return q, errors.New("invalid invite endpoint")
	}
	if p, err := strconv.Atoi(endpoint.Port()); err != nil || p <= 0 || p > 65535 {
		return q, errors.New("invalid invite port")
	}
	return q, nil
}

func directAddress(host string, port int) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/?#@\\ \n\r\t") || len(host) > 253 {
		return "", errors.New("enter a reachable public IP or hostname (no scheme)")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", errors.New("enter host without port; use the separate port field")
	}
	if port < 1 || port > 65535 {
		return "", errors.New("invalid direct port")
	}
	return "tcp://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (m *Mesh) newDirectInvite(host string, port int) (string, int, time.Time, error) {
	if port < 0 || port > 65535 {
		return "", 0, time.Time{}, errors.New("port must be 0–65535")
	}
	// Make the listener first so the real assigned ephemeral port can be encoded.
	l, e := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if e != nil {
		return "", 0, time.Time{}, e
	}
	port = l.Addr().(*net.TCPAddr).Port
	address, e := directAddress(host, port)
	if e != nil {
		_ = l.Close()
		return "", 0, time.Time{}, e
	}
	secret, e := directRandom(32)
	if e != nil {
		_ = l.Close()
		return "", 0, time.Time{}, e
	}
	expires := time.Now().Add(directTTL)
	q := directInvite{Version: 1, Address: address, Key: m.public, Secret: base64.RawURLEncoding.EncodeToString(secret), Expires: expires.Unix()}
	code, e := sealDirectInvite(q)
	if e != nil {
		_ = l.Close()
		return "", 0, time.Time{}, e
	}
	entry := &directPendingInvite{listener: l, expires: expires, code: code}
	copy(entry.secret[:], secret)
	m.directMu.Lock()
	if m.invite != nil {
		_ = m.invite.listener.Close()
	}
	m.invite = entry
	m.directMu.Unlock()
	go m.acceptDirect(entry)
	return code, port, expires, nil
}
func (m *Mesh) stopDirectInvite() {
	m.directMu.Lock()
	if m.invite != nil {
		_ = m.invite.listener.Close()
		m.invite = nil
	}
	m.directMu.Unlock()
}
func (m *Mesh) acceptDirect(entry *directPendingInvite) {
	timer := time.AfterFunc(time.Until(entry.expires), func() { _ = entry.listener.Close() })
	defer timer.Stop()
	workers := make(chan struct{}, 8)
	for {
		c, err := entry.listener.Accept()
		if err != nil {
			break
		}
		m.directMu.Lock()
		active := m.invite == entry && time.Now().Before(entry.expires)
		m.directMu.Unlock()
		if !active {
			_ = c.Close()
			break
		}
		// Unauthenticated clients cannot consume unbounded goroutines.
		select {
		case workers <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		go func(c net.Conn) {
			defer func() { <-workers }()
			if err := c.SetDeadline(time.Now().Add(7 * time.Second)); err != nil {
				_ = c.Close()
				return
			}
			wire, e := directHandshake(c, entry.secret[:], false)
			if e != nil {
				_ = c.Close()
				return
			}
			if !entry.claimed.CompareAndSwap(false, true) {
				_ = c.Close()
				return
			}
			m.directMu.Lock()
			stillOpen := m.invite == entry
			m.directMu.Unlock()
			if !stillOpen {
				_ = c.Close()
				return
			}
			sess, e := m.directIntroduce(wire, "")
			if e != nil {
				entry.claimed.Store(false)
				_ = c.Close()
				return
			}
			m.directMu.Lock()
			if m.invite != entry {
				m.directMu.Unlock()
				sess.close()
				return
			}
			m.invite = nil
			_ = entry.listener.Close() // closes LISTEN socket, not accepted TCP connection
			m.directMu.Unlock()
			sess.run()
		}(c)
	}
	m.directMu.Lock()
	if m.invite == entry {
		m.invite = nil
	}
	m.directMu.Unlock()
}
func (m *Mesh) joinDirect(ctx context.Context, code string) (string, error) {
	q, e := openDirectInvite(strings.TrimSpace(code), time.Now())
	if e != nil {
		return "", e
	}
	u, _ := url.Parse(q.Address)
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	c, e := dialer.DialContext(ctx, "tcp", u.Host)
	if e != nil {
		return "", fmt.Errorf("direct port unreachable: %w", e)
	}
	sec, _ := base64.RawURLEncoding.DecodeString(q.Secret)
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	wire, e := directHandshake(c, sec, true)
	if e != nil {
		_ = c.Close()
		return "", e
	}
	sess, e := m.directIntroduce(wire, q.Key)
	if e != nil {
		_ = c.Close()
		return "", e
	}
	sess.run()
	return sess.key, nil
}

// x25519 ephemeral ECDH prevents decrypting earlier tunnel traffic when an
// invite is exposed after use. PSK-HMAC authenticates the introduction.
func directHandshake(c net.Conn, secret []byte, client bool) (*directCipher, error) {
	curve := ecdh.X25519()
	priv, e := curve.GenerateKey(rand.Reader)
	if e != nil {
		return nil, e
	}
	pub := priv.PublicKey().Bytes()
	nonce, e := directRandom(16)
	if e != nil {
		return nil, e
	}
	var clientPub, serverPub, clientNonce, serverNonce []byte
	if client {
		intro := append(append([]byte(directMagic), pub...), nonce...)
		intro = append(intro, directMAC(secret, []byte("client"), intro)...)
		if _, e = c.Write(intro); e != nil {
			return nil, e
		}
		reply := make([]byte, 32+16+32)
		if _, e = io.ReadFull(c, reply); e != nil {
			return nil, e
		}
		clientPub, clientNonce = pub, nonce
		serverPub, serverNonce = reply[:32], reply[32:48]
		expected := directMAC(secret, []byte("server"), clientPub, serverPub, clientNonce, serverNonce)
		if !hmac.Equal(expected, reply[48:]) {
			return nil, errors.New("direct handshake authentication failed")
		}
	} else {
		intro := make([]byte, len(directMagic)+32+16+32)
		if _, e = io.ReadFull(c, intro); e != nil {
			return nil, e
		}
		if !bytes.Equal(intro[:len(directMagic)], []byte(directMagic)) {
			return nil, errors.New("invalid direct handshake")
		}
		expected := directMAC(secret, []byte("client"), intro[:len(directMagic)+48])
		if !hmac.Equal(expected, intro[len(directMagic)+48:]) {
			return nil, errors.New("direct invite authorization failed")
		}
		clientPub, clientNonce = intro[len(directMagic):len(directMagic)+32], intro[len(directMagic)+32:len(directMagic)+48]
		serverPub, serverNonce = pub, nonce
		reply := append(append(append([]byte(nil), serverPub...), serverNonce...), directMAC(secret, []byte("server"), clientPub, serverPub, clientNonce, serverNonce)...)
		if _, e = c.Write(reply); e != nil {
			return nil, e
		}
	}
	remotePub := serverPub
	if !client {
		remotePub = clientPub
	}
	remote, e := curve.NewPublicKey(remotePub)
	if e != nil {
		return nil, e
	}
	shared, e := priv.ECDH(remote)
	if e != nil {
		return nil, e
	}
	seed := directMAC(secret, []byte("ghost/direct/v1"), shared, clientPub, serverPub, clientNonce, serverNonce)
	ck := directMAC(seed, []byte("client->server"))
	sk := directMAC(seed, []byte("server->client"))
	if !client {
		ck, sk = sk, ck
	}
	a, e := aes.NewCipher(ck)
	if e != nil {
		return nil, e
	}
	b, e := aes.NewCipher(sk)
	if e != nil {
		return nil, e
	}
	enc, e := cipher.NewGCM(a)
	if e != nil {
		return nil, e
	}
	dec, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	_ = c.SetDeadline(time.Time{})
	return &directCipher{c: c, enc: enc, dec: dec}, nil
}
func (w *directCipher) send(frame directFrame) error {
	data, e := json.Marshal(frame)
	if e != nil {
		return e
	}
	if len(data) > directMaxFrame-64 {
		return errors.New("direct tunnel frame too large")
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], w.outSeq)
	cipherText := w.enc.Seal(nil, nonce[:], data, nil)
	w.outSeq++
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(cipherText)))
	_ = w.c.SetWriteDeadline(time.Now().Add(7 * time.Second))
	payload := append(size[:], cipherText...)
	for len(payload) != 0 {
		n, err := w.c.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
func (w *directCipher) receive() (directFrame, error) {
	var f directFrame
	_ = w.c.SetReadDeadline(time.Now().Add(35 * time.Second))
	var size [4]byte
	if _, e := io.ReadFull(w.c, size[:]); e != nil {
		return f, e
	}
	n := binary.BigEndian.Uint32(size[:])
	if n < 16 || n > directMaxFrame {
		return f, errors.New("invalid tunnel frame size")
	}
	body := make([]byte, int(n))
	if _, e := io.ReadFull(w.c, body); e != nil {
		return f, e
	}
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], w.inSeq)
	clear, e := w.dec.Open(nil, nonce[:], body, nil)
	if e != nil {
		return f, errors.New("tunnel frame authentication failed")
	}
	w.inSeq++
	if e = json.Unmarshal(clear, &f); e != nil {
		return f, e
	}
	return f, nil
}
func (m *Mesh) directHello() directFrame {
	users := m.activeUsers()
	f := directFrame{Type: "hello", Key: m.public, Users: users}
	content, _ := json.Marshal(struct {
		Key   string
		Users []MeshUser
	}{m.public, users})
	f.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(m.private, append([]byte("ghost-direct-hello-v1\n"), content...)))
	return f
}
func verifyDirectHello(f directFrame, pinned string) error {
	if f.Type != "hello" || f.Key == "" || (pinned != "" && f.Key != pinned) || len(f.Users) > peerUserLimit {
		return errors.New("unexpected Ghost node identity")
	}
	pub, e := base64.RawURLEncoding.DecodeString(f.Key)
	sig, es := base64.RawURLEncoding.DecodeString(f.Signature)
	content, _ := json.Marshal(struct {
		Key   string
		Users []MeshUser
	}{f.Key, f.Users})
	if e != nil || len(pub) != 32 || es != nil || !ed25519.Verify(pub, append([]byte("ghost-direct-hello-v1\n"), content...), sig) {
		return errors.New("bad direct node signature")
	}
	for _, u := range f.Users {
		if len(u.ID) != 10 || len(u.CallID) != 9 || len(u.Name) > 128 {
			return errors.New("invalid direct peer profile")
		}
	}
	return nil
}
func (m *Mesh) directIntroduce(wire *directCipher, expected string) (*directSession, error) {
	if e := wire.send(m.directHello()); e != nil {
		return nil, e
	}
	hello, e := wire.receive()
	if e != nil {
		return nil, e
	}
	if e = verifyDirectHello(hello, expected); e != nil {
		return nil, e
	}
	if hello.Key == m.public {
		return nil, errors.New("cannot pair Ghost with itself")
	}
	s := &directSession{mesh: m, wire: wire, key: hello.Key, done: make(chan struct{}), pending: make(map[uint64]chan directFrame), serveSlots: make(chan struct{}, 24)}
	m.directMu.Lock()
	if old := m.direct[hello.Key]; old != nil {
		old.close()
	}
	m.direct[hello.Key] = s
	m.directMu.Unlock()
	m.directRefreshPeer(hello.Key, hello.Users)
	return s, nil
}
func (m *Mesh) directRefreshPeer(key string, users []MeshUser) {
	m.mu.Lock()
	p := m.peers[key]
	if p == nil {
		p = &MeshPeer{Key: key, URL: "direct://" + key[:12]}
		m.peers[key] = p
	}
	previous := append([]MeshUser(nil), p.Users...)
	if p.URL == "" {
		p.URL = "direct://" + key[:12]
	}
	p.Users = users
	p.LastSeen = time.Now()
	m.mu.Unlock()
	m.h.mu.Lock()
	for _, u := range append(previous, users...) {
		m.h.notifyFriendsLocked(u.ID)
	}
	m.h.mu.Unlock()
}
func (s *directSession) run() {
	go s.readLoop()
	go func() {
		ticker := time.NewTicker(7 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				if e := s.wire.send(s.mesh.directHello()); e != nil {
					s.close()
					return
				}
			}
		}
	}()
}
func (s *directSession) close() { s.once.Do(func() { close(s.done); _ = s.wire.c.Close() }) }
func (s *directSession) readLoop() {
	defer func() {
		s.close()
		m := s.mesh
		m.directMu.Lock()
		wasCurrent := m.direct[s.key] == s
		if wasCurrent {
			delete(m.direct, s.key)
		}
		m.directMu.Unlock()
		if !wasCurrent {
			return
		}
		m.mu.Lock()
		p := m.peers[s.key]
		var users []MeshUser
		if p != nil && strings.HasPrefix(p.URL, "direct://") {
			p.LastSeen = time.Time{}
			users = append(users, p.Users...)
		}
		m.mu.Unlock()
		if len(users) != 0 {
			m.h.mu.Lock()
			for _, u := range users {
				m.h.notifyFriendsLocked(u.ID)
			}
			m.h.mu.Unlock()
		}
	}()
	for {
		f, e := s.wire.receive()
		if e != nil {
			return
		}
		switch f.Type {
		case "hello":
			if verifyDirectHello(f, s.key) != nil {
				return
			}
			s.mesh.directRefreshPeer(s.key, f.Users)
		case "request":
			if f.ID == 0 {
				return
			}
			select {
			case s.serveSlots <- struct{}{}:
				go func() { defer func() { <-s.serveSlots }(); s.serveRequest(f) }()
			default:
				_ = s.wire.send(directFrame{Type: "response", ID: f.ID, Status: 429, Message: "tunnel busy"})
			}
		case "response":
			s.mu.Lock()
			ch := s.pending[f.ID]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		default:
			return
		}
	}
}
func (s *directSession) serveRequest(f directFrame) {
	if !strings.HasPrefix(f.Path, "/p2p/") || len(f.Body) > 65536 {
		_ = s.wire.send(directFrame{Type: "response", ID: f.ID, Status: 400, Message: "bad path"})
		return
	}
	req, e := http.NewRequest("POST", "http://direct.local"+f.Path, bytes.NewReader(f.Body))
	if e != nil {
		return
	}
	req.Header.Set("X-Ghost-Node-Key", s.key)
	req.Header.Set("X-Ghost-Time", f.Time)
	req.Header.Set("X-Ghost-Nonce", f.Nonce)
	req.Header.Set("X-Ghost-Signature", f.Signature)
	recorder := httptest.NewRecorder()
	s.mesh.meshRoutes().ServeHTTP(recorder, req)
	resp := recorder.Result()
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_ = s.wire.send(directFrame{Type: "response", ID: f.ID, Status: resp.StatusCode, Message: string(body)})
}
func (s *directSession) request(ctx context.Context, path string, body []byte) error {
	ts, nonce, signature, e := s.mesh.signature("POST", path, body)
	if e != nil {
		return e
	}
	id := s.nextID.Add(1)
	ch := make(chan directFrame, 1)
	s.mu.Lock()
	if len(s.pending) >= 64 {
		s.mu.Unlock()
		return errors.New("direct tunnel busy")
	}
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	f := directFrame{Type: "request", ID: id, Path: path, Body: body, Time: ts, Nonce: nonce, Signature: signature}
	if e = s.wire.send(f); e != nil {
		s.close()
		return e
	}
	select {
	case <-s.done:
		return errors.New("direct tunnel disconnected")
	case <-ctx.Done():
		return ctx.Err()
	case resp := <-ch:
		if resp.Status != http.StatusOK {
			return fmt.Errorf("direct peer rejected %s (%d): %s", path, resp.Status, strings.TrimSpace(resp.Message))
		}
		return nil
	}
}
func (m *Mesh) directPost(ctx context.Context, key, path string, body []byte) (bool, error) {
	m.directMu.Lock()
	s := m.direct[key]
	m.directMu.Unlock()
	if s == nil {
		return false, nil
	}
	return true, s.request(ctx, path, body)
}
func (m *Mesh) closeDirect() {
	m.stopDirectInvite()
	m.directMu.Lock()
	sessions := make([]*directSession, 0, len(m.direct))
	for _, s := range m.direct {
		sessions = append(sessions, s)
	}
	m.directMu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}
func (m *Mesh) apiDirectInvite(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") || m.h.authenticate(w, r) == nil {
		return
	}
	var q struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	code, port, expires, e := m.newDirectInvite(q.Host, q.Port)
	if e != nil {
		http.Error(w, e.Error(), 400)
		return
	}
	sendJSON(w, 200, map[string]any{"code": code, "port": port, "expires": expires.UTC().Format(time.RFC3339)})
}
func (m *Mesh) apiDirectJoin(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") || m.h.authenticate(w, r) == nil {
		return
	}
	var q struct {
		Code string `json:"code"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	key, e := m.joinDirect(ctx, q.Code)
	if e != nil {
		http.Error(w, e.Error(), 400)
		return
	}
	sendJSON(w, 200, map[string]any{"connected": true, "nodeKey": key})
}
func (m *Mesh) apiDirectCancel(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") || m.h.authenticate(w, r) == nil {
		return
	}
	m.stopDirectInvite()
	sendJSON(w, 200, map[string]bool{"ok": true})
}
