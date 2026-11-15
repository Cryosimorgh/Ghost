package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const publicNTPServer = "time.cloudflare.com:123"

// An NTP UTC anchor advances using monotonic elapsed time, so changing the
// Windows clock cannot change advertisement ages or lease scheduling.
type synchronizedNetworkClock struct {
	mu              sync.Mutex
	utcAnchor       time.Time
	monotonicAnchor time.Time
	query           func(context.Context) (time.Time, error)
	server          string
}

var discoveryNetworkClock = synchronizedNetworkClock{query: queryPublicNTPTime}

func (c *synchronizedNetworkClock) cachedTime() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.monotonicAnchor.IsZero() {
		return time.Time{}
	}
	return c.utcAnchor.Add(time.Since(c.monotonicAnchor))
}

func (c *synchronizedNetworkClock) currentTime(ctx context.Context) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.monotonicAnchor.IsZero() && time.Since(c.monotonicAnchor) < 10*time.Minute {
		return c.utcAnchor.Add(time.Since(c.monotonicAnchor)), nil
	}
	utc, err := c.query(ctx)
	if err != nil {
		server := c.server
		if server == "" {
			server = publicNTPServer
		}
		return time.Time{}, fmt.Errorf("public NTP synchronization (%s): %w", server, err)
	}
	c.utcAnchor, c.monotonicAnchor = utc, time.Now()
	return utc, nil
}

func queryPublicNTPTime(ctx context.Context) (time.Time, error) {
	return queryNTPTime(ctx, publicNTPServer)
}

func queryNTPTime(ctx context.Context, server string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", server)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return time.Time{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var request [48]byte
	request[0] = 0x23
	if _, err := rand.Read(request[40:48]); err != nil {
		return time.Time{}, err
	}
	started := time.Now()
	if _, err := conn.Write(request[:]); err != nil {
		return time.Time{}, err
	}
	var response [512]byte
	n, err := conn.Read(response[:])
	if err != nil {
		return time.Time{}, err
	}
	return parseNTPResponse(request, response[:n], time.Since(started))
}

func parseNTPResponse(request [48]byte, response []byte, roundTrip time.Duration) (time.Time, error) {
	if len(response) < 48 {
		return time.Time{}, io.ErrUnexpectedEOF
	}
	version := (response[0] >> 3) & 7
	if response[0]>>6 == 3 || (version != 3 && version != 4) || response[0]&7 != 4 || response[1] == 0 || response[1] > 15 {
		return time.Time{}, errors.New("unsynchronized or invalid NTP server reply")
	}
	for i := 0; i < 8; i++ {
		if response[24+i] != request[40+i] {
			return time.Time{}, errors.New("NTP reply does not match request")
		}
	}
	receive := decodeNTPTimestamp(response[32:40])
	transmit := decodeNTPTimestamp(response[40:48])
	if allZero(response[32:40]) || allZero(response[40:48]) || transmit.Before(receive) || transmit.Sub(receive) > roundTrip {
		return time.Time{}, errors.New("invalid NTP server timestamps")
	}
	return transmit.Add((roundTrip - transmit.Sub(receive)) / 2), nil
}

func decodeNTPTimestamp(data []byte) time.Time {
	seconds := int64(binary.BigEndian.Uint32(data[:4]))
	const epochOffset = 2208988800
	if seconds < epochOffset {
		seconds += 1 << 32
	} // NTP era rollover in 2036, without the local clock.
	nanos := (uint64(binary.BigEndian.Uint32(data[4:])) * 1000000000) >> 32
	return time.Unix(seconds-epochOffset, int64(nanos)).UTC()
}
