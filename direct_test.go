package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDirectInviteSecrecyTamperAndExpiry(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := newHub(filepath.Join(t.TempDir(), "ghost.json"))
	m := newMesh(h, key)
	h.mesh = m
	code, port, exp, err := m.newDirectInvite("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.stopDirectInvite()
	if port <= 0 || !strings.HasPrefix(code, directPrefix) || strings.Contains(code, "127.0.0.1") {
		t.Fatal("invalid encoded invite")
	}
	q, err := openDirectInvite(code, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if q.Key != m.public || q.Expires != exp.Unix() {
		t.Fatal("wrong node identity or expiry")
	}
	corrupt := []byte(code)
	corrupt[len(corrupt)-5] ^= 1
	if _, err = openDirectInvite(string(corrupt), time.Now()); err == nil {
		t.Fatal("tamper accepted")
	}
	if _, err = openDirectInvite(code, exp.Add(time.Second)); err == nil {
		t.Fatal("expired invite accepted")
	}
	if _, err = openDirectInvite(code, time.Now().Add(-24*time.Hour)); err == nil {
		t.Fatal("future-dated invite accepted")
	}
}
func TestIndependentNodesDirectInviteTunnelAndOneTimePort(t *testing.T) {
	mk := func(name string) (*Hub, *Mesh, *httptest.Server, testIdentity, *Session) {
		h := newHub(filepath.Join(t.TempDir(), name+".json"))
		_, pk, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		m := newMesh(h, pk)
		h.mesh = m
		s := httptest.NewServer(h.routes())
		person := newPerson(t, s, name)
		call, _ := randomID(9)
		session := &Session{UserID: person.ID, ID: person.ID + "-session-1234567890", CallID: call, Started: time.Now(), Events: make(chan Event, 20)}
		h.mu.Lock()
		h.sessions[session.ID] = session
		h.calls[call] = session
		h.mu.Unlock()
		return h, m, s, person, session
	}
	ha, ma, sa, alice, aliceSession := mk("Alice")
	hb, mb, sb, bob, bobSession := mk("Bob")
	defer sa.Close()
	defer sb.Close()
	defer ma.closeDirect()
	defer mb.closeDirect()
	// The normal HTTP peer service is deliberately not started on either node.
	code, port, _, e := ma.newDirectInvite("127.0.0.1", 0)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, e := mb.joinDirect(ctx, code)
	if e != nil {
		t.Fatalf("cannot join remote node: %v", e)
	}
	if key != ma.public {
		t.Fatal("remote node identity not pinned")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ma.directMu.Lock()
		serverReady := ma.direct[mb.public] != nil
		pending := ma.invite
		ma.directMu.Unlock()
		if serverReady && pending == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("one-use listener did not close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmtPort(port)), 100*time.Millisecond)
	if e == nil {
		c.Close()
		t.Fatal("single-use listening socket still open")
	}
	if _, e = mb.joinDirect(ctx, code); e == nil {
		t.Fatal("reused invite was accepted")
	}
	if peer, u := mb.findUser(alice.ID); peer == nil || u.CallID != aliceSession.CallID {
		t.Fatalf("missing Alice profile: %+v", u)
	}
	if peer, u := ma.findUser(bob.ID); peer == nil || u.CallID != bobSession.CallID {
		t.Fatalf("missing Bob profile: %+v", u)
	}
	// Friend requests and responses travel both ways over the persistent encrypted socket.
	testAPI(t, sb, "POST", "/api/friend/request", bob, map[string]string{"id": alice.ID}, 200)
	a := testAPI(t, sa, "GET", "/api/friends", alice, nil, 200)
	if len(a["incoming"].([]any)) != 1 {
		t.Fatal("friend request not delivered over tunnel")
	}
	testAPI(t, sa, "POST", "/api/friend/respond", alice, map[string]any{"id": bob.ID, "accept": true}, 200)
	b := testAPI(t, sb, "GET", "/api/friends", bob, nil, 200)
	if len(b["friends"].([]any)) != 1 {
		t.Fatal("friend acceptance not delivered over tunnel")
	}
	// Signaling is routed over the same socket without invoking HTTP peer-listen.
	testAPI(t, sb, "POST", "/api/signal", bob, map[string]any{"to": aliceSession.CallID, "kind": "invite"}, 200)
	deadlineSignal := time.After(2 * time.Second)
forSignal:
	for {
		select {
		case event := <-aliceSession.Events:
			if event.Type == "signal" {
				break forSignal
			}
		case <-deadlineSignal:
			t.Fatal("signal failed across tunnel")
		}
	}
	_ = ha
	_ = hb
}
func fmtPort(v int) string { return strconv.Itoa(v) }
