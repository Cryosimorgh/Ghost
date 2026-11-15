package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func startTestProxy(t *testing.T, protocol, upstream, early string) (*url.URL, <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	targets := make(chan string, 8)
	var mu sync.Mutex
	active := map[net.Conn]bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			active[conn] = true
			mu.Unlock()
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				defer func() { mu.Lock(); delete(active, c); mu.Unlock() }()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(c)
				first, err := reader.Peek(1)
				if err != nil {
					return
				}
				if first[0] == 5 {
					if protocol == "http" {
						_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
						return
					}
					var head [2]byte
					if _, err := io.ReadFull(reader, head[:]); err != nil {
						return
					}
					if _, err := io.CopyN(io.Discard, reader, int64(head[1])); err != nil {
						return
					}
					_, _ = c.Write([]byte{5, 0})
					var request [5]byte
					if _, err := io.ReadFull(reader, request[:]); err != nil {
						return
					}
					if request[0] != 5 || request[1] != 1 || request[3] != 3 {
						t.Error("SOCKS destination did not retain remote DNS")
						return
					}
					name := make([]byte, int(request[4])+2)
					if _, err := io.ReadFull(reader, name); err != nil {
						return
					}
					targets <- net.JoinHostPort(string(name[:len(name)-2]), itoaPort(binary.BigEndian.Uint16(name[len(name)-2:])))
					_, _ = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				} else {
					req, err := http.ReadRequest(reader)
					if err != nil {
						return
					}
					if req.Method != http.MethodConnect {
						t.Error("proxy request is not CONNECT")
						return
					}
					targets <- req.Host
					_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"+early)
				}
				if upstream == "" {
					_, _ = io.Copy(c, reader)
					return
				}
				remote, err := net.DialTimeout("tcp", upstream, time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				defer remote.Close()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(remote, reader); _ = remote.Close(); close(done) }()
				_, _ = io.Copy(c, remote)
				_ = c.Close()
				<-done
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for c := range active {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	p, _ := url.Parse(protocol + "://" + l.Addr().String())
	return p, targets
}

func itoaPort(p uint16) string { return strconv.Itoa(int(p)) }

func TestProxyTunnelPreservesHostnameAndData(t *testing.T) {
	for _, protocol := range []string{"socks5", "http", "auto"} {
		t.Run(protocol, func(t *testing.T) {
			p, targets := startTestProxy(t, protocol, "", "")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c, err := dialNetworkProxy(ctx, p, "unresolvable.test:443")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, _ = io.WriteString(c, "proxy roundtrip")
			data := make([]byte, len("proxy roundtrip"))
			if _, err := io.ReadFull(c, data); err != nil || string(data) != "proxy roundtrip" {
				t.Fatalf("tunnel data corrupted: %s %v", data, err)
			}
			if got := <-targets; got != "unresolvable.test:443" {
				t.Fatalf("proxy resolved/rewrote the destination: %s", got)
			}
		})
	}
}

func TestUntypedHTTPProxyAndBufferedTunnelBytes(t *testing.T) {
	p, targets := startTestProxy(t, "http", "", "ready")
	p.Scheme = "auto"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := dialNetworkProxy(ctx, p, "unresolvable.test:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ready" {
		t.Fatalf("CONNECT discarded buffered bytes: %s %v", buf, err)
	}
	if got := <-targets; got != "unresolvable.test:443" {
		t.Fatal(got)
	}
}

func TestTLSStillVerifiesTheDestinationThroughSOCKS(t *testing.T) {
	d := startJSONLServer(t, true, func(r directoryRequest) []byte { return jsonLine(map[string]any{"ok": false, "error": "not found"}) })
	address := net.JoinHostPort("127.0.0.1", itoaPort(uint16(d.port)))
	p, _ := startTestProxy(t, "socks5", address, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := dialNetworkProxy(ctx, p, net.JoinHostPort("localhost", itoaPort(uint16(d.port))))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	cfg := d.tlsConfig.Clone()
	cfg.ServerName = "localhost"
	secure := tls.Client(raw, cfg)
	if err := secure.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = secure.Write(jsonLine(directoryRequest{Op: "get", Key: "probe"}))
	line, err := bufio.NewReader(secure).ReadString('\n')
	if err != nil || !strings.Contains(line, "not found") {
		t.Fatalf("JSONL did not reach the TLS service: %s %v", line, err)
	}
}

func TestProxyCancellationInterruptsNegotiation(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	p, _ := url.Parse("socks5://" + l.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = dialNetworkProxy(ctx, p, "unresolvable.test:443")
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("proxy cancellation ignored: %v", err)
	}
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(time.Second):
		t.Fatal("not connected")
	}
}

func TestSystemProxyParsingAndBypass(t *testing.T) {
	target, _ := url.Parse("https://www.soulless.ai:443")
	for _, tc := range []struct{ server, bypass, want string }{
		{"127.0.0.1:10808", "", "auto://127.0.0.1:10808"},
		{"http=127.0.0.1:8080;https=127.0.0.1:10808", "", "auto://127.0.0.1:10808"},
		{"socks=127.0.0.1:10808", "", "socks5://127.0.0.1:10808"},
		{"127.0.0.1:10808", "*.soulless.ai", ""},
	} {
		p, err := parseSystemProxy(tc.server, tc.bypass, target)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if p != nil {
			got = p.String()
		}
		if got != tc.want {
			t.Fatalf("proxy config %s => %s, want %s", tc.server, got, tc.want)
		}
	}
	for _, address := range []string{"127.0.0.1:8788", "localhost:8788", "192.168.1.3:8788", "[::1]:8788"} {
		if p, err := configuredNetworkProxy(address); err != nil || p != nil {
			t.Fatalf("local connection sent through proxy: %s %v", address, err)
		}
	}
	if !noProxyAddress("www.soulless.ai:443", ".soulless.ai") || !noProxyAddress("192.168.1.3:8788", "192.168.0.0/16") {
		t.Fatal("NO_PROXY matching failed")
	}
}
