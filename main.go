package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webAssets embed.FS

type User struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	TokenHash string   `json:"token_hash"`
	Friends   []string `json:"friends"`
	Incoming  []string `json:"incoming"`
	Outgoing  []string `json:"outgoing"`
}
type State struct {
	Users map[string]*User `json:"users"`
}
type Session struct {
	UserID  string
	ID      string
	CallID  string
	Started time.Time
	Events  chan Event
}
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}
type Hub struct {
	mu                          sync.Mutex
	state                       State
	path                        string
	sessions                    map[string]*Session // session token -> live connection
	calls                       map[string]*Session // ephemeral call ID -> connection
	stun                        []string
	turnURL, turnUser, turnPass string
}
type Friend struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	CallID string `json:"callId,omitempty"`
}
type FriendState struct {
	Friends  []Friend `json:"friends"`
	Incoming []Friend `json:"incoming"`
	Outgoing []Friend `json:"outgoing"`
}

func randomID(n int) (string, error) {
	b := make([]byte, (n*5+7)/8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), "=")[:n], nil
}
func token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func remove(xs []string, v string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
func newHub(path string) *Hub {
	return &Hub{path: path, state: State{Users: make(map[string]*User)}, sessions: make(map[string]*Session), calls: make(map[string]*Session), stun: []string{"stun:stun.l.google.com:19302", "stun:stun.cloudflare.com:3478"}}
}
func (h *Hub) load() error {
	b, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &h.state); err != nil {
		return fmt.Errorf("invalid state file: %w", err)
	}
	if h.state.Users == nil {
		h.state.Users = make(map[string]*User)
	}
	return nil
}
func (h *Hub) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(h.state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(h.path), ".ghost-*.tmp")
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
	return os.Rename(f.Name(), h.path)
}
func (h *Hub) user(r *http.Request) *User {
	id := strings.ToUpper(strings.TrimSpace(r.Header.Get("X-User-ID")))
	secret := r.Header.Get("X-Auth-Token")
	if len(secret) != 64 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.state.Users[id]
	if u == nil {
		return nil
	}
	expected, err := hex.DecodeString(u.TokenHash)
	if err != nil {
		return nil
	}
	actual := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
		return nil
	}
	copy := *u
	return &copy
}
func (h *Hub) authenticate(w http.ResponseWriter, r *http.Request) *User {
	u := h.user(r)
	if u == nil {
		http.Error(w, "Invalid identity; restore your access key or create a new identity.", http.StatusUnauthorized)
	}
	return u
}
func sendJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "Invalid request body", 400)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		http.Error(w, "Unexpected extra JSON", 400)
		return false
	}
	return true
}
func method(w http.ResponseWriter, r *http.Request, m string) bool {
	if r.Method != m {
		w.Header().Set("Allow", m)
		http.Error(w, "Method not allowed", 405)
		return false
	}
	return true
}
func (h *Hub) register(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	var q struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	q.Name = strings.TrimSpace(q.Name)
	if len([]rune(q.Name)) < 1 || len([]rune(q.Name)) > 32 {
		http.Error(w, "Name must contain 1–32 characters", 400)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var id, secret string
	var err error
	for {
		id, err = randomID(10)
		if err != nil {
			http.Error(w, "Entropy failure", 500)
			return
		}
		if h.state.Users[id] == nil {
			break
		}
	}
	secret, err = token()
	if err != nil {
		http.Error(w, "Entropy failure", 500)
		return
	}
	h.state.Users[id] = &User{ID: id, Name: q.Name, TokenHash: hash(secret), Friends: []string{}, Incoming: []string{}, Outgoing: []string{}}
	if err = h.saveLocked(); err != nil {
		delete(h.state.Users, id)
		log.Printf("save registration: %v", err)
		http.Error(w, "Could not save identity", 500)
		return
	}
	sendJSON(w, 201, map[string]string{"id": id, "name": q.Name, "token": secret})
}
func (h *Hub) me(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	u := h.authenticate(w, r)
	if u == nil {
		return
	}
	sendJSON(w, 200, map[string]string{"id": u.ID, "name": u.Name})
}
func (h *Hub) onlineLocked(userID string) (string, bool) {
	var latest *Session
	for _, s := range h.sessions {
		if s.UserID == userID && (latest == nil || s.Started.After(latest.Started)) {
			latest = s
		}
	}
	if latest == nil {
		return "", false
	}
	return latest.CallID, true
}
func (h *Hub) makeFriendLocked(id string) Friend {
	u := h.state.Users[id]
	if u == nil {
		return Friend{ID: id, Name: "Unknown"}
	}
	call, online := h.onlineLocked(id)
	return Friend{ID: u.ID, Name: u.Name, Online: online, CallID: call}
}
func (h *Hub) friendsLocked(id string) FriendState {
	st := FriendState{Friends: []Friend{}, Incoming: []Friend{}, Outgoing: []Friend{}}
	u := h.state.Users[id]
	if u == nil {
		return st
	}
	for _, fid := range u.Friends {
		st.Friends = append(st.Friends, h.makeFriendLocked(fid))
	}
	for _, fid := range u.Incoming {
		st.Incoming = append(st.Incoming, h.makeFriendLocked(fid))
	}
	for _, fid := range u.Outgoing {
		st.Outgoing = append(st.Outgoing, h.makeFriendLocked(fid))
	}
	sort.Slice(st.Friends, func(i, j int) bool {
		if st.Friends[i].Online != st.Friends[j].Online {
			return st.Friends[i].Online
		}
		return st.Friends[i].Name < st.Friends[j].Name
	})
	return st
}
func (h *Hub) friends(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	u := h.authenticate(w, r)
	if u == nil {
		return
	}
	h.mu.Lock()
	st := h.friendsLocked(u.ID)
	h.mu.Unlock()
	sendJSON(w, 200, st)
}
func (h *Hub) emitLocked(userID string, e Event) {
	for _, s := range h.sessions {
		if s.UserID == userID {
			select {
			case s.Events <- e:
			default: /* saturated client: drop hint; next refresh catches up */
			}
		}
	}
}
func (h *Hub) notifyFriendsLocked(id string) {
	u := h.state.Users[id]
	if u == nil {
		return
	}
	for _, fid := range u.Friends {
		h.emitLocked(fid, Event{Type: "presence", Data: map[string]string{"userId": id}})
	}
}
func (h *Hub) requestFriend(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	me := h.authenticate(w, r)
	if me == nil {
		return
	}
	var q struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	q.ID = strings.ToUpper(strings.TrimSpace(q.ID))
	h.mu.Lock()
	defer h.mu.Unlock()
	other := h.state.Users[q.ID]
	u := h.state.Users[me.ID]
	if other == nil || other.ID == u.ID {
		http.Error(w, "No such other Mail ID", 404)
		return
	}
	if contains(u.Friends, other.ID) {
		http.Error(w, "Already friends", 409)
		return
	}
	if contains(u.Incoming, other.ID) {
		http.Error(w, "They already sent you an invite. Accept it from your inbox.", 409)
		return
	}
	if contains(u.Outgoing, other.ID) {
		sendJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(other.Incoming) >= 100 || len(u.Outgoing) >= 100 {
		http.Error(w, "Inbox is full", 429)
		return
	}
	u.Outgoing = append(u.Outgoing, other.ID)
	other.Incoming = append(other.Incoming, u.ID)
	if err := h.saveLocked(); err != nil {
		u.Outgoing = remove(u.Outgoing, other.ID)
		other.Incoming = remove(other.Incoming, u.ID)
		http.Error(w, "Storage failure", 500)
		return
	}
	h.emitLocked(other.ID, Event{Type: "inbox", Data: nil})
	h.emitLocked(u.ID, Event{Type: "inbox", Data: nil})
	sendJSON(w, 200, map[string]bool{"ok": true})
}
func (h *Hub) respondFriend(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	me := h.authenticate(w, r)
	if me == nil {
		return
	}
	var q struct {
		ID     string `json:"id"`
		Accept bool   `json:"accept"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	q.ID = strings.ToUpper(strings.TrimSpace(q.ID))
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.state.Users[me.ID]
	other := h.state.Users[q.ID]
	if other == nil || !contains(u.Incoming, q.ID) {
		http.Error(w, "Invitation not found", 404)
		return
	}
	u.Incoming = remove(u.Incoming, other.ID)
	other.Outgoing = remove(other.Outgoing, u.ID)
	if q.Accept {
		if !contains(u.Friends, other.ID) {
			u.Friends = append(u.Friends, other.ID)
		}
		if !contains(other.Friends, u.ID) {
			other.Friends = append(other.Friends, u.ID)
		}
	}
	if err := h.saveLocked(); err != nil {
		log.Printf("save friend response: %v", err)
		http.Error(w, "Storage failure", 500)
		return
	}
	h.emitLocked(other.ID, Event{Type: "inbox", Data: nil})
	h.emitLocked(u.ID, Event{Type: "inbox", Data: nil})
	sendJSON(w, 200, map[string]bool{"ok": true})
}
func (h *Hub) events(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	u := h.authenticate(w, r)
	if u == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unavailable", 500)
		return
	}
	sessionID := strings.TrimSpace(r.Header.Get("X-Session-ID"))
	if len(sessionID) < 16 || len(sessionID) > 128 {
		http.Error(w, "Invalid session ID", 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	h.mu.Lock()
	if prior := h.sessions[sessionID]; prior != nil {
		if prior.UserID != u.ID {
			h.mu.Unlock()
			http.Error(w, "Session conflict", 409)
			return
		}
		delete(h.calls, prior.CallID)
		delete(h.sessions, sessionID)
	}
	var callID string
	for {
		callID, _ = randomID(9)
		if h.calls[callID] == nil {
			break
		}
	}
	s := &Session{UserID: u.ID, ID: sessionID, CallID: callID, Started: time.Now(), Events: make(chan Event, 96)}
	h.sessions[sessionID] = s
	h.calls[callID] = s
	st := h.friendsLocked(u.ID)
	h.notifyFriendsLocked(u.ID)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.sessions[sessionID] == s {
			delete(h.sessions, sessionID)
			delete(h.calls, callID)
			h.notifyFriendsLocked(u.ID)
		}
		h.mu.Unlock()
	}()
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	write := func(e Event) bool {
		b, err := json.Marshal(e)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		if err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(Event{Type: "hello", Data: map[string]any{"callId": callID, "friends": st}}) {
		return
	}
	ticker := time.NewTicker(22 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-s.Events:
			if !write(e) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
func (h *Hub) signal(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	u := h.authenticate(w, r)
	if u == nil {
		return
	}
	var q struct {
		To   string          `json:"to"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	allowed := map[string]bool{"invite": true, "accept": true, "decline": true, "offer": true, "answer": true, "ice": true, "hangup": true}
	if !allowed[q.Kind] || len(q.Data) > 60000 {
		http.Error(w, "Invalid signal", 400)
		return
	}
	q.To = strings.ToUpper(strings.TrimSpace(q.To))
	sid := r.Header.Get("X-Session-ID")
	h.mu.Lock()
	defer h.mu.Unlock()
	from := h.sessions[sid]
	if from == nil || from.UserID != u.ID {
		http.Error(w, "Not connected to presence stream", 409)
		return
	}
	to := h.calls[q.To]
	if to == nil || to == from {
		http.Error(w, "Call ID is offline", 404)
		return
	}
	message := map[string]any{"fromCallId": from.CallID, "fromUserId": u.ID, "fromName": u.Name, "kind": q.Kind, "data": q.Data}
	select {
	case to.Events <- Event{Type: "signal", Data: message}:
		sendJSON(w, 200, map[string]bool{"ok": true})
	default:
		http.Error(w, "Recipient is busy", 429)
	}
}
func (h *Hub) config(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	servers := []map[string]any{}
	if len(h.stun) > 0 {
		servers = append(servers, map[string]any{"urls": h.stun})
	}
	if h.turnURL != "" {
		servers = append(servers, map[string]any{"urls": strings.Split(h.turnURL, ","), "username": h.turnUser, "credential": h.turnPass})
	}
	sendJSON(w, 200, map[string]any{"iceServers": servers})
}
func (h *Hub) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", h.register)
	mux.HandleFunc("/api/me", h.me)
	mux.HandleFunc("/api/config", h.config)
	mux.HandleFunc("/api/friends", h.friends)
	mux.HandleFunc("/api/friend/request", h.requestFriend)
	mux.HandleFunc("/api/friend/respond", h.respondFriend)
	mux.HandleFunc("/api/events", h.events)
	mux.HandleFunc("/api/signal", h.signal)
	assets, _ := fs.Sub(webAssets, "web")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := webAssets.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "UI unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; media-src 'self' blob:; worker-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.Method == "POST" && r.Header.Get("Origin") != "" && r.Header.Get("Origin") != originFor(r) {
			http.Error(w, "Cross-origin request rejected", 403)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func originFor(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func main() {
	tray := flag.Bool("tray", runtime.GOOS == "windows", "show Ghost in the Windows notification area; includes Open and Exit")
	open := flag.Bool("open", runtime.GOOS == "windows" || (runtime.GOOS == "linux" && (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "")), "open the local web interface after starting the hub")
	listen := flag.String("listen", "127.0.0.1:8787", "listen address; use 0.0.0.0:8787 to accept remote clients")
	data := flag.String("data", "ghost-data.json", "hub identity and friend mailbox database (not message history)")
	cert := flag.String("tls-cert", "", "TLS certificate PEM (for remote mic access)")
	key := flag.String("tls-key", "", "TLS private key PEM")
	stun := flag.String("stun", "stun:stun.l.google.com:19302,stun:stun.cloudflare.com:3478", "comma-separated STUN server URLs")
	turn := flag.String("turn-url", "", "TURN URLs, comma-separated (e.g. turn:relay.example.org:3478)")
	turnUser := flag.String("turn-user", "", "TURN username")
	turnPass := flag.String("turn-pass", "", "TURN credential")
	flag.Parse()
	if (*cert == "") != (*key == "") {
		log.Fatal("Provide both -tls-cert and -tls-key")
	}
	h := newHub(*data)
	if err := h.load(); err != nil {
		log.Fatalf("Cannot load hub state: %v", err)
	}
	h.stun = nil
	for _, s := range strings.Split(*stun, ",") {
		if t := strings.TrimSpace(s); t != "" {
			h.stun = append(h.stun, t)
		}
	}
	h.turnURL, h.turnUser, h.turnPass = *turn, *turnUser, *turnPass
	srv := &http.Server{Addr: *listen, Handler: h.routes(), ReadHeaderTimeout: 8 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return context.Background() }}
	protocol := "http"
	if *cert != "" {
		protocol = "https"
	}
	log.Printf("Ghost hub at %s://%s (data: %s)", protocol, *listen, *data)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	if *open || *tray {
		address := listener.Addr().(*net.TCPAddr)
		host := "127.0.0.1"
		if address.IP.IsLoopback() && address.IP.To4() != nil {
			host = address.IP.String()
		}
		url := fmt.Sprintf("%s://%s:%d/", protocol, host, address.Port)
		if *tray {
			startTray(url, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			})
		}
		if *open {
			go func() {
				time.Sleep(250 * time.Millisecond)
				openBrowser(url)
			}()
		}
	}
	if *cert != "" {
		err = srv.ServeTLS(listener, *cert, *key)
	} else {
		err = srv.Serve(listener)
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("Could not open browser: %v", err)
	}
}
