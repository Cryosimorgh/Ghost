package main

// The optional directory only exchanges signed 128-byte endpoint advertisements.
// It is not a Ghost hub, message store, signaling proxy, STUN server, or relay.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const directoryDomain = "ghost-directory-v1\x00"
const directoryRecordSize = 128

type directoryClient struct {
	host        string
	publishPort int
	lookupPort  int
	tls         bool
}

func newDirectoryClient(host string, publishPort, lookupPort int, secure bool) (*directoryClient, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.Contains(host, "://") || strings.ContainsAny(host, "/\\@ ") || publishPort < 1 || publishPort > 65535 || lookupPort < 1 || lookupPort > 65535 || publishPort == lookupPort {
		return nil, errors.New("directory requires a hostname/IP and two distinct valid TCP ports")
	}
	return &directoryClient{host: host, publishPort: publishPort, lookupPort: lookupPort, tls: secure}, nil
}
func directoryID(public ed25519.PublicKey) int64 {
	sum := sha256.Sum256(public)
	return int64(binary.BigEndian.Uint64(sum[:8]) & (uint64(1)<<63 - 1))
}
func (d *directoryClient) exchange(ctx context.Context, port int, request []byte, response []byte) error {
	address := net.JoinHostPort(d.host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 4 * time.Second}
	var conn net.Conn
	var err error
	if d.tls {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: d.host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(6 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	for len(request) > 0 {
		n, e := conn.Write(request)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		request = request[n:]
	}
	_, err = io.ReadFull(conn, response)
	return err
}
func packDirectoryRecord(private ed25519.PrivateKey, rawURL string, timestamp uint32) ([directoryRecordSize]byte, error) {
	var record [directoryRecordSize]byte
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return record, errors.New("invalid -peer-advertise URL")
	}
	host := parsed.Hostname()
	port := uint16(0)
	if parsed.Port() == "" {
		if parsed.Scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	} else {
		p, e := strconv.ParseUint(parsed.Port(), 10, 16)
		if e != nil || p == 0 {
			return record, errors.New("invalid port")
		}
		port = uint16(p)
	}
	pub := private.Public().(ed25519.PublicKey)
	copy(record[:32], pub)
	record[32] = 1
	kind := byte(0)
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			kind = 1
			copy(record[40:44], ipv4)
		} else {
			kind = 2
			copy(record[40:56], ip.To16())
		}
	} else {
		host = strings.ToLower(host)
		if len(host) > 24 || len(host) == 0 || strings.HasPrefix(host, "-") || strings.HasSuffix(host, "-") {
			return record, errors.New("directory DNS hostname must be <= 24 ASCII characters; use IP or short alias")
		}
		for i := 0; i < len(host); i++ {
			c := host[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
				return record, errors.New("invalid directory hostname")
			}
		}
		kind = 3
		copy(record[40:64], host)
	}
	if parsed.Scheme == "https" {
		kind |= 0x80
	}
	record[33] = kind
	binary.BigEndian.PutUint16(record[34:36], port)
	binary.BigEndian.PutUint32(record[36:40], timestamp)
	signature := ed25519.Sign(private, append([]byte(directoryDomain), record[:64]...))
	copy(record[64:], signature)
	return record, nil
}
func unpackDirectoryRecord(record [directoryRecordSize]byte, expectedID int64) (string, string, error) {
	if record == ([directoryRecordSize]byte{}) {
		return "", "", errors.New("directory ID not found or expired")
	}
	pub := ed25519.PublicKey(record[:32])
	if expectedID <= 0 || directoryID(pub) != expectedID || !ed25519.Verify(pub, append([]byte(directoryDomain), record[:64]...), record[64:]) {
		return "", "", errors.New("directory ID or signature is invalid")
	}
	ep := record[32:64]
	version := ep[0]
	kind := ep[1]
	if version != 1 || (kind&0x7f) < 1 || (kind&0x7f) > 3 {
		return "", "", errors.New("invalid directory endpoint")
	}
	port := binary.BigEndian.Uint16(ep[2:4])
	timestamp := int64(binary.BigEndian.Uint32(ep[4:8]))
	now := time.Now().Unix()
	if port == 0 || timestamp > now+300 || timestamp < now-1860 {
		return "", "", errors.New("stale or invalid directory endpoint")
	}
	var host string
	switch kind & 0x7f {
	case 1:
		if !allZero(ep[12:]) {
			return "", "", errors.New("invalid IPv4 padding")
		}
		host = net.IP(ep[8:12]).String()
	case 2:
		if !allZero(ep[24:]) {
			return "", "", errors.New("invalid IPv6 padding")
		}
		host = net.IP(ep[8:24]).String()
	case 3:
		host = strings.TrimRight(string(ep[8:]), "\x00")
		if host == "" || len(host) > 24 || !allZero(ep[8+len(host):]) {
			return "", "", errors.New("invalid DNS endpoint")
		}
		for i := 0; i < len(host); i++ {
			c := host[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
				return "", "", errors.New("invalid DNS hostname")
			}
		}
	}
	scheme := "http"
	if kind&0x80 != 0 {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(int(port))), base64.RawURLEncoding.EncodeToString(pub), nil
}
func allZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}
func (d *directoryClient) publish(ctx context.Context, private ed25519.PrivateKey, advertise string) (int64, error) {
	record, err := packDirectoryRecord(private, advertise, uint32(time.Now().Unix()))
	if err != nil {
		return 0, err
	}
	var reply [8]byte
	if err = d.exchange(ctx, d.publishPort, record[:], reply[:]); err != nil {
		return 0, err
	}
	id := int64(binary.BigEndian.Uint64(reply[:]))
	if id == 0 || id != directoryID(private.Public().(ed25519.PublicKey)) {
		return 0, errors.New("directory rejected advertisement or capacity exceeded")
	}
	return id, nil
}
func (d *directoryClient) resolveDetails(ctx context.Context, id int64) (string, string, uint32, error) {
	if id <= 0 {
		return "", "", 0, errors.New("invalid directory code")
	}
	var request [8]byte
	binary.BigEndian.PutUint64(request[:], uint64(id))
	var response [directoryRecordSize]byte
	if err := d.exchange(ctx, d.lookupPort, request[:], response[:]); err != nil {
		return "", "", 0, err
	}
	url, key, err := unpackDirectoryRecord(response, id)
	if err != nil {
		return "", "", 0, err
	}
	return url, key, binary.BigEndian.Uint32(response[36:40]), nil
}
func (d *directoryClient) resolve(ctx context.Context, id int64) (string, string, error) {
	url, key, _, err := d.resolveDetails(ctx, id)
	return url, key, err
}
func (m *Mesh) startDirectory(d *directoryClient, advertise string) {
	m.mu.Lock()
	m.directory = d
	m.mu.Unlock()
	if d == nil || advertise == "" {
		return
	}
	go func() {
		check := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			id := directoryID(m.private.Public().(ed25519.PublicKey))
			if url, _, timestamp, err := d.resolveDetails(ctx, id); err == nil && url == advertise && time.Since(time.Unix(int64(timestamp), 0)) < 8*time.Minute {
				m.mu.Lock()
				m.directoryCode = id
				m.mu.Unlock()
				return
			}
			id, err := d.publish(ctx, m.private, advertise)
			m.mu.Lock()
			if err == nil {
				m.directoryCode = id
			} else {
				m.directoryCode = 0
			}
			m.mu.Unlock()
			if err != nil {
				fmt.Printf("Ghost directory: publish failed: %v\n", err)
			}
		}
		check()
		ticker := time.NewTicker(3 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				check()
			case <-m.stop:
				return
			}
		}
	}()
}
