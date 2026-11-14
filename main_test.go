package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testIdentity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

func testAPI(t *testing.T, server *httptest.Server, method, path string, who testIdentity, payload any, expected int) map[string]any {
	t.Helper()
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User-ID", who.ID)
	req.Header.Set("X-Auth-Token", who.Token)
	req.Header.Set("X-Session-ID", who.ID+"-session-1234567890")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != expected {
		t.Fatalf("%s %s: status %d want %d", method, path, res.StatusCode, expected)
	}
	if res.StatusCode >= 400 {
		return nil
	}
	var parsed map[string]any
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}
func newPerson(t *testing.T, s *httptest.Server, name string) testIdentity {
	t.Helper()
	r := testAPI(t, s, "POST", "/api/register", testIdentity{}, map[string]string{"name": name}, 201)
	return testIdentity{ID: r["id"].(string), Name: r["name"].(string), Token: r["token"].(string)}
}
func TestRegistrationAndFriendMailbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	h := newHub(path)
	s := httptest.NewServer(h.routes())
	defer s.Close()
	alice := newPerson(t, s, "Alice")
	bob := newPerson(t, s, "Bob")
	if len(alice.ID) != 10 || len(alice.Token) != 64 {
		t.Fatal("invalid generated credentials")
	}
	testAPI(t, s, "GET", "/api/me", alice, nil, 200)
	testAPI(t, s, "GET", "/api/me", testIdentity{ID: alice.ID, Token: "wrong"}, nil, 401)
	testAPI(t, s, "POST", "/api/friend/request", alice, map[string]string{"id": bob.ID}, 200)
	state := testAPI(t, s, "GET", "/api/friends", bob, nil, 200)
	if len(state["incoming"].([]any)) != 1 {
		t.Fatalf("expected 1 invitation: %v", state)
	}
	testAPI(t, s, "POST", "/api/friend/respond", bob, map[string]any{"id": alice.ID, "accept": true}, 200)
	for _, who := range []testIdentity{alice, bob} {
		state = testAPI(t, s, "GET", "/api/friends", who, nil, 200)
		if len(state["friends"].([]any)) != 1 {
			t.Fatalf("expected mutual friendship: %v", state)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm()&0077 != 0 {
		t.Fatalf("state permissions: %v %v", fi, err)
	}
	saved := newHub(path)
	if err := saved.load(); err != nil {
		t.Fatal(err)
	}
	if len(saved.state.Users) != 2 || !contains(saved.state.Users[bob.ID].Friends, alice.ID) {
		t.Fatal("friendship did not survive restart")
	}
}
func startEvents(t *testing.T, server *httptest.Server, who testIdentity) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest("GET", server.URL+"/api/events", nil)
	req.Header.Set("X-User-ID", who.ID)
	req.Header.Set("X-Auth-Token", who.Token)
	req.Header.Set("X-Session-ID", who.ID+"-session-1234567890")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		t.Fatalf("event stream status %d", res.StatusCode)
	}
	return res, bufio.NewReader(res.Body)
}
func nextEvent(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			var result map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &result); err != nil {
				t.Fatal(err)
			}
			return result
		}
	}
}
func TestLiveCallIdsAndSignaling(t *testing.T) {
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	s := httptest.NewServer(h.routes())
	defer s.Close()
	a := newPerson(t, s, "Alpha")
	b := newPerson(t, s, "Beta")
	ra, ar := startEvents(t, s, a)
	defer ra.Body.Close()
	helloA := nextEvent(t, ar)
	rb, br := startEvents(t, s, b)
	defer rb.Body.Close()
	helloB := nextEvent(t, br)
	aCall := helloA["data"].(map[string]any)["callId"].(string)
	bCall := helloB["data"].(map[string]any)["callId"].(string)
	if aCall == bCall || len(aCall) != 9 || len(bCall) != 9 {
		t.Fatal("incorrect live call IDs")
	}
	testAPI(t, s, "POST", "/api/signal", a, map[string]any{"to": bCall, "kind": "invite", "data": nil}, 200)
	signal := nextEvent(t, br)
	if signal["type"] != "signal" || signal["data"].(map[string]any)["fromCallId"] != aCall {
		t.Fatalf("bad routed signaling: %v", signal)
	}
	testAPI(t, s, "POST", "/api/signal", b, map[string]any{"to": aCall, "kind": "accept", "data": nil}, 200)
	if nextEvent(t, ar)["type"] != "signal" {
		t.Fatal("acceptance was not routed")
	}
	testAPI(t, s, "POST", "/api/signal", a, map[string]any{"to": "XXXXXXXXX", "kind": "invite"}, 404)
}
func TestRequestValidation(t *testing.T) {
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	s := httptest.NewServer(h.routes())
	defer s.Close()
	alice := newPerson(t, s, "Alice")
	testAPI(t, s, "POST", "/api/friend/request", alice, map[string]string{"id": alice.ID}, 404)
	testAPI(t, s, "POST", "/api/register", testIdentity{}, map[string]string{"name": ""}, 400)
	request, _ := http.NewRequest("POST", s.URL+"/api/friend/request", bytes.NewBufferString(fmt.Sprintf(`{"id":"%s"}`, alice.ID)))
	request.Header.Set("X-User-ID", alice.ID)
	request.Header.Set("X-Auth-Token", alice.Token)
	request.Header.Set("Origin", "http://untrusted.example")
	c := http.Client{Timeout: time.Second}
	resp, err := c.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("cross-origin request not rejected")
	}
}

func TestFriendPresenceExposesLiveCallID(t *testing.T) {
	h := newHub(filepath.Join(t.TempDir(), "hub.json"))
	s := httptest.NewServer(h.routes())
	defer s.Close()
	a := newPerson(t, s, "A")
	b := newPerson(t, s, "B")
	testAPI(t, s, "POST", "/api/friend/request", a, map[string]string{"id": b.ID}, 200)
	testAPI(t, s, "POST", "/api/friend/respond", b, map[string]any{"id": a.ID, "accept": true}, 200)

	state := testAPI(t, s, "GET", "/api/friends", a, nil, 200)
	bf := state["friends"].([]any)[0].(map[string]any)
	if bf["online"] != false || bf["callId"] != nil {
		t.Fatalf("offline friend exposed a call id: %v", bf)
	}

	r, events := startEvents(t, s, b)
	defer r.Body.Close()
	hello := nextEvent(t, events)
	bCall := hello["data"].(map[string]any)["callId"].(string)
	state = testAPI(t, s, "GET", "/api/friends", a, nil, 200)
	bf = state["friends"].([]any)[0].(map[string]any)
	if bf["online"] != true || bf["callId"] != bCall {
		t.Fatalf("online friend has no live call id: %v", bf)
	}

	if err := r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state = testAPI(t, s, "GET", "/api/friends", a, nil, 200)
		bf = state["friends"].([]any)[0].(map[string]any)
		if bf["online"] == false && bf["callId"] == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("presence did not clear after session ended: %v", bf)
}
