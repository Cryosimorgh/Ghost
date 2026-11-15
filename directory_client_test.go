package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDirectoryRecordAndTampering(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"http://203.0.113.7:8788", "https://[2001:db8::7]:8788", "https://ghost.example.org:8788"}
	for _, name := range names {
		rec, e := packDirectoryRecord(key, name, uint32(time.Now().Unix()))
		if e != nil {
			t.Fatal(e)
		}
		id := directoryID(key.Public().(ed25519.PublicKey))
		got, public, e := unpackDirectoryRecord(rec, id)
		if e != nil || got != name || public != base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) {
			t.Fatalf("bad roundtrip %q, %q %v", got, public, e)
		}
		rec[40] ^= 0x02
		if _, _, e = unpackDirectoryRecord(rec, id); e == nil {
			t.Fatal("tampering was accepted")
		}
	}
	_, err = packDirectoryRecord(key, "https://a-very-very-very-long-hostname.example.org:8788", uint32(time.Now().Unix()))
	if err == nil {
		t.Fatal("hostname overflow accepted")
	}
}
func TestDirectoryClientProtocol(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	id := directoryID(key.Public().(ed25519.PublicKey))
	gotRecord := make(chan [128]byte, 1)
	go func() {
		c, e := l1.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		var record [128]byte
		_, e = io.ReadFull(c, record[:])
		if e != nil {
			return
		}
		gotRecord <- record
		var reply [8]byte
		binary.BigEndian.PutUint64(reply[:], uint64(id))
		_, _ = c.Write(reply[:])
	}()
	go func() {
		c, e := l2.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		var request [8]byte
		_, e = io.ReadFull(c, request[:])
		if e != nil || int64(binary.BigEndian.Uint64(request[:])) != id {
			return
		}
		rec, e := packDirectoryRecord(key, "http://203.0.113.7:8788", uint32(time.Now().Unix()))
		if e == nil {
			_, _ = c.Write(rec[:])
		}
	}()
	dc := &directoryClient{host: "127.0.0.1", publishPort: l1.Addr().(*net.TCPAddr).Port, lookupPort: l2.Addr().(*net.TCPAddr).Port}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	published, e := dc.publish(ctx, key, "http://203.0.113.7:8788")
	if e != nil || published != id {
		t.Fatalf("publish %d %v", published, e)
	}
	record := <-gotRecord
	if !bytes.Equal(record[:32], key.Public().(ed25519.PublicKey)) {
		t.Fatal("wrong node key")
	}
	endpoint, public, e := dc.resolve(ctx, id)
	if e != nil || endpoint != "http://203.0.113.7:8788" || public == "" {
		t.Fatalf("resolve %s %v", endpoint, e)
	}
	if _, e = newDirectoryClient("127.0.0.1", 777, 777, false); e == nil {
		t.Fatal("accepted same-port config")
	}
	if _, e = newDirectoryClient("http://example.com", 777, 778, false); e == nil {
		t.Fatal("accepted scheme in host")
	}
	if _, e = strconv.Atoi("not-a-number"); e == nil {
		t.Fatal("test bug")
	}
}

// Run with a real Python directory server listening on localhost to cross-check
// the Go Ed25519 record format against the Python verifier, end to end.
func TestPythonDirectoryAcrossIndependentNodes(t *testing.T) {
	portOne := os.Getenv("GHOST_DIR_PUBLISH_PORT")
	portTwo := os.Getenv("GHOST_DIR_LOOKUP_PORT")
	if portOne == "" || portTwo == "" {
		t.Skip("set directory port environment variables for Python integration")
	}
	p1, _ := strconv.Atoi(portOne)
	p2, _ := strconv.Atoi(portTwo)
	directory, err := newDirectoryClient("127.0.0.1", p1, p2, false)
	if err != nil {
		t.Fatal(err)
	}
	a := newHub(filepath.Join(t.TempDir(), "nodea.json"))
	b := newHub(filepath.Join(t.TempDir(), "nodeb.json"))
	_, keyA, _ := ed25519.GenerateKey(rand.Reader)
	_, keyB, _ := ed25519.GenerateKey(rand.Reader)
	ma := newMesh(a, keyA)
	mb := newMesh(b, keyB)
	a.mesh = ma
	b.mesh = mb
	if err = ma.start("127.0.0.1:0", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	defer ma.close()
	if err = mb.start("127.0.0.1:0", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	defer mb.close()
	code, err := directory.publish(context.Background(), keyA, ma.url)
	if err != nil {
		t.Fatal(err)
	}
	mb.directory = directory
	server := httptest.NewServer(b.routes())
	defer server.Close()
	user := newPerson(t, server, "Directory tester")
	testAPI(t, server, "POST", "/api/peers/connect", user, map[string]string{"address": strconv.FormatInt(code, 10)}, 200)
	if peer := mb.peer(ma.public); peer == nil || peer.URL != ma.url {
		t.Fatalf("directory node was not discovered: %+v", peer)
	}
}
