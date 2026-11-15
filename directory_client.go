package main

// Discovery is JSONL over TCP with optional verified TLS. There is no HTTP
// layer. The store is opaque; Ghost verifies signed peer values on lookup.
import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const directoryDomain = "ghost-directory-v1\x00"
const directoryRecordSize = 128
const directoryMaxLine = 4096
const directoryKeyPrefix = "ghost:v1:"

type directoryClient struct {
	host        string
	port        int
	tls         bool
	tlsConfig   *tls.Config // optional trusted test CA; never disables verification
	dialer      *net.Dialer
	diagnostics *networkDiagnostics
}

func newDirectoryClient(host string, publishPort, lookupPort int, secure bool) (*directoryClient, error) {
	host = strings.TrimSpace(host)
	// Accept the supplied address label, while connecting only to its hostname.
	// JSONL over TCP/TLS has no HTTP request path.
	host = strings.TrimSuffix(strings.TrimSuffix(host, "/v/api/ghost/"), "/v/api/ghost")
	if host == "" || strings.ContainsAny(host, "/\\@?# \t\r\n") || (strings.Contains(host, ":") && net.ParseIP(strings.Trim(host, "[]")) == nil) {
		return nil, errors.New("discovery requires a TCP hostname/IP, optionally followed by /v/api/ghost; do not enter an HTTP URL")
	}
	if publishPort < 1 || publishPort > 65535 || lookupPort != publishPort {
		return nil, errors.New("discovery uses one TCP port for set/get; both stored port values must match (e.g. 443)")
	}
	return &directoryClient{host: strings.Trim(host, "[]"), port: publishPort, tls: secure, diagnostics: &networkDiagnostics{}}, nil
}
func directoryID(public ed25519.PublicKey) int64 {
	sum := sha256.Sum256(public)
	return int64(binary.BigEndian.Uint64(sum[:8]) & (uint64(1)<<63 - 1))
}
func directoryKey(id int64) string { return directoryKeyPrefix + strconv.FormatInt(id, 10) }

type directoryRequest struct {
	Op    string `json:"op"`
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}
type directoryReply struct {
	OK      *bool   `json:"ok"`
	Key     string  `json:"key"`
	Value   *string `json:"value"`
	Expires float64 `json:"expires"`
	Error   string  `json:"error"`
	Deleted *bool   `json:"deleted"`
}
type directoryServerError struct{ reason string }

func (e *directoryServerError) Error() string { return "discovery server: " + e.reason }
func directoryMissing(err error) bool {
	var e *directoryServerError
	return errors.As(err, &e) && (e.reason == "not found" || e.reason == "expired")
}
func (d *directoryClient) begin(ctx context.Context, stage string) (context.Context, *networkTrace) {
	diag := d.diagnostics
	if diag == nil {
		diag = &networkDiagnostics{}
	}
	return diag.begin(ctx, stage)
}
func (d *directoryClient) do(ctx context.Context, request directoryRequest) (directoryReply, error) {
	ctx, trace := d.begin(ctx, "directory."+request.Op)
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var reply directoryReply
	if request.Op != "get" && request.Op != "set" && request.Op != "delete" {
		return reply, trace.fail("directory.protocol", errors.New("unsupported discovery operation"))
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return reply, trace.fail("directory.encode", err)
	}
	if len(payload)+1 > directoryMaxLine {
		return reply, trace.fail("directory.encode", errors.New("discovery request exceeds 4096-byte line limit"))
	}
	address := net.JoinHostPort(d.host, strconv.Itoa(d.port))
	transport := "TCP; TLS disabled"
	if d.tls {
		transport = "TCP + TLS; certificate verification enabled; SNI=" + d.host
	}
	trace.event("directory.transport", "start", transport+"; destination="+address)
	trace.event("directory.tcp", "start", "Connecting to "+address)
	var raw net.Conn
	if d.dialer != nil {
		raw, err = d.dialer.DialContext(ctx, "tcp", address)
	} else {
		raw, err = dialNetwork(ctx, "tcp", address, trace)
	}
	if err != nil {
		var dns *net.DNSError
		if errors.As(err, &dns) {
			return reply, trace.fail("directory.dns", err)
		}
		return reply, trace.fail("directory.tcp", err)
	}
	defer raw.Close()
	trace.event("directory.tcp", "ok", "Connected to "+address)
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return reply, trace.fail("directory.deadline", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	var conn net.Conn = raw
	if d.tls {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if d.tlsConfig != nil {
			cfg = d.tlsConfig.Clone()
		}
		cfg.ServerName = d.host
		if cfg.MinVersion < tls.VersionTLS12 {
			cfg.MinVersion = tls.VersionTLS12
		}
		cfg.InsecureSkipVerify = false
		secureConn := tls.Client(raw, cfg)
		trace.event("directory.tls", "start", "TLS handshake; verifying certificate for "+d.host)
		if err := secureConn.HandshakeContext(ctx); err != nil {
			return reply, trace.fail("directory.tls", err)
		}
		trace.event("directory.tls", "ok", tls.VersionName(secureConn.ConnectionState().Version)+"; certificate verified; SNI="+d.host)
		conn = secureConn
	}
	trace.event("directory.json.write", "start", fmt.Sprintf("op=%s; key=%s; %d JSONL bytes (value omitted)", request.Op, request.Key, len(payload)+1))
	pending := append(payload, '\n')
	for len(pending) > 0 {
		n, err := conn.Write(pending)
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return reply, trace.fail("directory.json.write", err)
		}
		if n == 0 {
			return reply, trace.fail("directory.json.write", io.ErrShortWrite)
		}
		pending = pending[n:]
	}
	trace.event("directory.json.read", "start", "Waiting for one newline-terminated JSON reply")
	line, err := bufio.NewReaderSize(conn, directoryMaxLine).ReadSlice('\n')
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			err = errors.New("discovery reply exceeds 4096-byte line limit")
		}
		return reply, trace.fail("directory.json.read", err)
	}
	if err := json.Unmarshal(line, &reply); err != nil || reply.OK == nil {
		if strings.HasPrefix(string(line), "HTTP/") {
			status := "HTTP"
			parts := strings.Fields(string(line))
			if len(parts) > 1 {
				if code, err := strconv.Atoi(parts[1]); err == nil && code >= 100 && code <= 599 {
					status = fmt.Sprintf("HTTP %d", code)
				}
			}
			return reply, trace.fail("directory.protocol", fmt.Errorf("discovery listener returned %s instead of JSONL; check nginx routing to the Python discovery socket", status))
		}
		return reply, trace.fail("directory.json.decode", errors.New("expected one JSON object with an ok boolean (reply payload omitted)"))
	}
	if !*reply.OK {
		reason := reply.Error
		if reason == "" {
			return reply, trace.fail("directory.json.decode", errors.New("server error reply is missing its error field"))
		}
		if len(reason) > 256 {
			reason = reason[:256]
		}
		e := &directoryServerError{reason: reason}
		if directoryMissing(e) {
			trace.event("directory."+request.Op, "missing", reason)
			return reply, e
		}
		return reply, trace.fail("directory."+request.Op, e)
	}
	if reply.Key != request.Key {
		return reply, trace.fail("directory.json.verify", errors.New("reply key does not exactly match the requested key"))
	}
	trace.event("directory.json.read", "ok", fmt.Sprintf("op=%s; key matched; %d JSONL bytes (value omitted)", request.Op, len(line)))
	return reply, nil
}
func directoryExpiry(reply directoryReply) (time.Time, error) {
	if math.IsNaN(reply.Expires) || math.IsInf(reply.Expires, 0) || reply.Expires <= float64(time.Now().UnixNano())/1e9 || reply.Expires > float64(1<<63-1)/1e9 {
		return time.Time{}, errors.New("server returned a missing or expired lease; check system clocks")
	}
	seconds, fraction := math.Modf(reply.Expires)
	return time.Unix(int64(seconds), int64(fraction*1e9)), nil
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
func (d *directoryClient) publishWithLease(ctx context.Context, private ed25519.PrivateKey, advertise string) (int64, time.Time, error) {
	ctx, trace := d.begin(ctx, "directory.publish")
	record, err := packDirectoryRecord(private, advertise, uint32(time.Now().Unix()))
	if err != nil {
		return 0, time.Time{}, trace.fail("directory.record", err)
	}
	id := directoryID(private.Public().(ed25519.PublicKey))
	value := base64.RawURLEncoding.EncodeToString(record[:])
	reply, err := d.do(ctx, directoryRequest{Op: "set", Key: directoryKey(id), Value: value})
	if err != nil {
		return 0, time.Time{}, err
	}
	expiry, err := directoryExpiry(reply)
	if err != nil {
		return 0, time.Time{}, trace.fail("directory.lease", err)
	}
	trace.event("directory.publish", "ok", fmt.Sprintf("Signed peer endpoint stored; code=%d; lease expires=%s", id, expiry.UTC().Format(time.RFC3339Nano)))
	return id, expiry, nil
}
func (d *directoryClient) publish(ctx context.Context, private ed25519.PrivateKey, advertise string) (int64, error) {
	id, _, err := d.publishWithLease(ctx, private, advertise)
	return id, err
}
func (d *directoryClient) resolveDetails(ctx context.Context, id int64) (string, string, uint32, error) {
	ctx, trace := d.begin(ctx, "directory.lookup")
	if id <= 0 {
		return "", "", 0, trace.fail("directory.lookup", errors.New("invalid directory code"))
	}
	reply, err := d.do(ctx, directoryRequest{Op: "get", Key: directoryKey(id)})
	if err != nil {
		return "", "", 0, err
	}
	if _, err := directoryExpiry(reply); err != nil {
		return "", "", 0, trace.fail("directory.lease", err)
	}
	if reply.Value == nil {
		return "", "", 0, trace.fail("directory.record", errors.New("get reply is missing the value field"))
	}
	data, err := base64.RawURLEncoding.DecodeString(*reply.Value)
	if err != nil || len(data) != directoryRecordSize {
		return "", "", 0, trace.fail("directory.record", errors.New("stored value is not a Ghost signed peer advertisement"))
	}
	var record [directoryRecordSize]byte
	copy(record[:], data)
	endpoint, key, err := unpackDirectoryRecord(record, id)
	if err != nil {
		return "", "", 0, trace.fail("directory.verify", err)
	}
	trace.event("directory.verify", "ok", "Signed peer endpoint and node ID verified: "+endpoint)
	return endpoint, key, binary.BigEndian.Uint32(record[36:40]), nil
}
func (d *directoryClient) resolve(ctx context.Context, id int64) (string, string, error) {
	endpoint, key, _, err := d.resolveDetails(ctx, id)
	return endpoint, key, err
}
func directoryRefreshDelay(expiry time.Time) time.Duration {
	delay := time.Until(expiry) / 2
	if delay > 3*time.Minute {
		delay = 3 * time.Minute
	}
	if delay < 100*time.Millisecond {
		delay = 100 * time.Millisecond
	}
	return delay
}
func (m *Mesh) startDirectory(d *directoryClient, advertise string) {
	m.mu.Lock()
	m.directory = d
	m.directoryStatus = "Lookup only"
	if advertise != "" {
		m.directoryStatus = "Publishing"
	}
	m.mu.Unlock()
	if d == nil || advertise == "" {
		return
	}
	go func() {
		lifetime, stop := context.WithCancel(context.Background())
		defer stop()
		go func() {
			select {
			case <-m.stop:
				stop()
			case <-lifetime.Done():
			}
		}()
		check := func() time.Duration {
			ctx, cancel := context.WithTimeout(lifetime, 15*time.Second)
			defer cancel()
			ctx, trace := d.begin(ctx, "directory.refresh")
			id, expiry, err := d.publishWithLease(ctx, m.private, advertise)
			if err == nil {
				endpoint, public, resolveErr := d.resolve(ctx, id)
				record, packErr := packDirectoryRecord(m.private, advertise, uint32(time.Now().Unix()))
				expected, _, unpackErr := unpackDirectoryRecord(record, id)
				if resolveErr != nil {
					err = resolveErr
				} else if packErr != nil || unpackErr != nil || endpoint != expected || public != m.public {
					err = trace.fail("directory.readback", errors.New("signed advertisement readback does not match this node"))
				}
			}
			m.mu.Lock()
			if err == nil {
				m.directoryCode = id
				m.directoryStatus = "Published"
				m.directoryError = ""
			} else {
				m.directoryCode = 0
				m.directoryStatus = "Publication failed"
				m.directoryError = err.Error()
			}
			m.mu.Unlock()
			if err != nil {
				return 15 * time.Second
			}
			delay := directoryRefreshDelay(expiry)
			trace.event("directory.refresh", "ok", fmt.Sprintf("Verified publication; refreshing lease in %s", delay.Round(time.Millisecond)))
			return delay
		}
		timer := time.NewTimer(check())
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				timer.Reset(check())
			case <-m.stop:
				return
			}
		}
	}()
}
