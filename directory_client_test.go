package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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
