package main

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Offline fixtures use a deterministic UTC anchor; production never installs it.
	discoveryNetworkClock.utcAnchor = time.Now()
	discoveryNetworkClock.monotonicAnchor = time.Now()
	os.Exit(m.Run())
}
func TestNetworkClockUsesNTPAnchorAndCachesQueries(t *testing.T) {
	expected := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	clock := synchronizedNetworkClock{query: func(context.Context) (time.Time, error) { calls++; return expected, nil }}
	for i := 0; i < 2; i++ {
		got, err := clock.currentTime(context.Background())
		if err != nil || got.Before(expected) || got.Sub(expected) > time.Second {
			t.Fatalf("network UTC anchor lost: %s %v", got, err)
		}
	}
	if calls != 1 {
		t.Fatal("NTP query was not cached")
	}
}
func TestNetworkClockDoesNotFallBackToLocalTime(t *testing.T) {
	clock := synchronizedNetworkClock{query: func(context.Context) (time.Time, error) { return time.Time{}, errors.New("NTP unavailable") }}
	if got, err := clock.currentTime(context.Background()); err == nil || !got.IsZero() {
		t.Fatal("unavailable NTP silently used local time")
	}
}
func TestNTPReplyValidationAndEraRollover(t *testing.T) {
	var request [48]byte
	request[40] = 123
	reply := make([]byte, 48)
	reply[0] = 0x24
	reply[1] = 2
	copy(reply[24:32], request[40:])
	expected := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, offset := range []int{32, 40} {
		binary.BigEndian.PutUint32(reply[offset:], uint32(expected.Unix()+2208988800))
	}
	got, err := parseNTPResponse(request, reply, 20*time.Millisecond)
	if err != nil || !got.Equal(expected.Add(10*time.Millisecond)) {
		t.Fatalf("invalid era decoding: %s %v", got, err)
	}
	reply[24] ^= 1
	if _, err := parseNTPResponse(request, reply, time.Second); err == nil {
		t.Fatal("unmatched NTP request accepted")
	}
	reply[24] ^= 1
	reply[1] = 0
	if _, err := parseNTPResponse(request, reply, time.Second); err == nil {
		t.Fatal("kiss-of-death response accepted")
	}
	reply[1] = 2
	reply[0] |= 0xc0
	if _, err := parseNTPResponse(request, reply, time.Second); err == nil {
		t.Fatal("unsynchronized server accepted")
	}
}
func TestDiscoveryExpiryUsesNetworkTime(t *testing.T) {
	networkUTC := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := directoryExpiry(directoryReply{Expires: float64(networkUTC.Unix() + 60)}, networkUTC); err != nil {
		t.Fatal(err)
	}
	if _, err := directoryExpiry(directoryReply{Expires: float64(networkUTC.Unix() - 1)}, networkUTC); err == nil {
		t.Fatal("expired network lease accepted")
	}
}
