package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndependentGhostNodesRouteFriendsAndSignals(t *testing.T) {
	var hs [2]*Hub
	var ms [2]*Mesh
	var ss [2]*httptest.Server
	var id [2]testIdentity
	for i := 0; i < 2; i++ {
		h := newHub(filepath.Join(t.TempDir(), "hub.json"))
		hs[i] = h
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		m := newMesh(h, key)
		h.mesh = m
		ms[i] = m
		if err := m.start("127.0.0.1:0", "", "", "", false); err != nil {
			t.Fatal(err)
		}
		defer m.close()
		s := httptest.NewServer(h.routes())
		ss[i] = s
		defer s.Close()
		id[i] = newPerson(t, s, []string{"Alice", "Bob"}[i])
	}
	// Separate local browsers, separate state files, separate network listeners.
	aRes, aRead := startEvents(t, ss[0], id[0])
	defer aRes.Body.Close()
	bRes, bRead := startEvents(t, ss[1], id[1])
	defer bRes.Body.Close()
	aHello := nextEvent(t, aRead)
	bHello := nextEvent(t, bRead)
	aCall := aHello["data"].(map[string]any)["callId"].(string)
	bCall := bHello["data"].(map[string]any)["callId"].(string)
	if aCall == bCall {
		t.Fatal("unexpected call ID collision")
	}
	testAPI(t, ss[0], "POST", "/api/peers/connect", id[0], map[string]string{"address": ms[1].url}, 200)
	for i := 0; i < 2; i++ {
		if ms[i].peer(ms[1-i].public) == nil {
			t.Fatalf("node %d did not discover its reciprocal peer", i)
		}
	}
	// Bob has no usable inbound URL for Alice; all replies must traverse
	// Alice's signed outbound polling connection, not a callback to Alice.
	if got := ms[1].peer(ms[0].public).URL; got != "" {
		t.Fatalf("expected outbound-only peer registration, got %s", got)
	}
	testAPI(t, ss[0], "POST", "/api/friend/request", id[0], map[string]string{"id": id[1].ID}, 200)
	inbox := testAPI(t, ss[1], "GET", "/api/friends", id[1], nil, 200)
	if len(inbox["incoming"].([]any)) != 1 {
		t.Fatalf("remote inbox did not receive friend request: %v", inbox)
	}
	testAPI(t, ss[1], "POST", "/api/friend/respond", id[1], map[string]any{"id": id[0].ID, "accept": true}, 200)
	if err := ms[0].pollOne(context.Background(), ms[1].url, ms[1].public); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		f := testAPI(t, ss[i], "GET", "/api/friends", id[i], nil, 200)
		friends := f["friends"].([]any)
		if len(friends) != 1 {
			t.Fatalf("node %d not friends: %v", i, f)
		}
		friend := friends[0].(map[string]any)
		if !friend["online"].(bool) {
			t.Fatalf("node %d remote friend not online: %v", i, f)
		}
	}
	testAPI(t, ss[0], "POST", "/api/signal", id[0], map[string]any{"to": bCall, "kind": "invite"}, 200)
	received := nextEvent(t, bRead)
	for received["type"] != "signal" {
		received = nextEvent(t, bRead)
	}
	data := received["data"].(map[string]any)
	if data["fromUserId"] != id[0].ID || data["fromCallId"] != aCall || data["kind"] != "invite" {
		t.Fatalf("wrong cross-node signal: %v", data)
	}
	testAPI(t, ss[1], "POST", "/api/signal", id[1], map[string]any{"to": aCall, "kind": "accept"}, 200)
	if err := ms[0].pollOne(context.Background(), ms[1].url, ms[1].public); err != nil {
		t.Fatal(err)
	}
	received = nextEvent(t, aRead)
	for received["type"] != "signal" {
		received = nextEvent(t, aRead)
	}
	if received["data"].(map[string]any)["fromCallId"] != bCall {
		t.Fatal("reply routed to wrong caller")
	}
	if len(hs[0].state.Users) != 2 || len(hs[1].state.Users) != 2 {
		t.Fatal("remote profiles were not recorded locally")
	}
}
func TestMeshSignatureReplayAndKeyPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	k, err := meshKey(path)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := meshKey(path)
	if err != nil || !bytes.Equal(k, k2) {
		t.Fatal("node key not persistent")
	}
	if f, err := os.Stat(path); err != nil || f.Mode().Perm()&0077 != 0 {
		t.Fatalf("insecure node key: %v %v", f, err)
	}
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	m := newMesh(h, k)
	h.mesh = m
	if err := m.start("127.0.0.1:0", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	defer m.close()
	body, _ := json.Marshal(map[string]string{"url": m.url})
	req, _ := http.NewRequest("POST", m.url+"/p2p/poll", bytes.NewReader(body))
	res, err := m.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("unsigned request accepted %d", res.StatusCode)
	}
	ts, nonce, sig, err := m.signature("POST", "/p2p/poll", body)
	if err != nil {
		t.Fatal(err)
	}
	signed := func() int {
		r, _ := http.NewRequest("POST", m.url+"/p2p/poll", bytes.NewReader(body))
		r.Header.Set("X-Ghost-Node-Key", m.public)
		r.Header.Set("X-Ghost-Time", ts)
		r.Header.Set("X-Ghost-Nonce", nonce)
		r.Header.Set("X-Ghost-Signature", sig)
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		response, e := m.client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if code := signed(); code != 200 {
		t.Fatalf("first signed poll should succeed, got %d", code)
	}
	if code := signed(); code != 409 {
		t.Fatalf("replayed request not rejected: %d", code)
	}
}

// Opt-in real-multicast smoke test; CI environments frequently block multicast.
func TestLANMulticastDiscovery(t *testing.T) {
	if os.Getenv("GHOST_TEST_LAN") != "1" {
		t.Skip("set GHOST_TEST_LAN=1 on a LAN-enabled host")
	}
	hs := make([]*Hub, 2)
	ms := make([]*Mesh, 2)
	for i := range hs {
		hs[i] = newHub(filepath.Join(t.TempDir(), "hub.json"))
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		ms[i] = newMesh(hs[i], priv)
		hs[i].mesh = ms[i]
		if err = ms[i].start("0.0.0.0:0", "", "", "", true); err != nil {
			t.Fatal(err)
		}
		defer ms[i].close()
	}
	deadline := time.Now().Add(13 * time.Second)
	for time.Now().Before(deadline) {
		if ms[0].peer(ms[1].public) != nil && ms[1].peer(ms[0].public) != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no multicast discovery: firewall, network policy, or multicast interface may block LAN announcements")
}
