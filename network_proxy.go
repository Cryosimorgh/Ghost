package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Discovery remains JSONL inside verified TLS. Proxy negotiation happens only
// with the user's configured proxy, before the connection reaches discovery.
func configuredNetworkProxy(address string) (*url.URL, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(host, "localhost") {
		return nil, nil
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return nil, nil
	}
	target := &url.URL{Scheme: "https", Host: address}
	if noProxyAddress(address, os.Getenv("NO_PROXY")) || noProxyAddress(address, os.Getenv("no_proxy")) {
		return nil, nil
	}
	proxy, err := http.ProxyFromEnvironment(&http.Request{URL: target})
	if err != nil || proxy != nil {
		return proxy, err
	}
	return systemNetworkProxy(target)
}

func noProxyAddress(address, list string) bool {
	host, port, _ := net.SplitHostPort(address)
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(strings.ToLower(item))
		if item == "" {
			continue
		}
		if item == "*" {
			return true
		}
		if _, network, err := net.ParseCIDR(item); err == nil {
			if ip := net.ParseIP(host); ip != nil && network.Contains(ip) {
				return true
			}
			continue
		}
		if h, p, err := net.SplitHostPort(item); err == nil {
			if p != port {
				continue
			}
			item = h
		}
		item = strings.TrimPrefix(strings.TrimPrefix(item, "*"), ".")
		if strings.EqualFold(host, item) || strings.HasSuffix(strings.ToLower(host), "."+item) {
			return true
		}
	}
	return false
}

func parseSystemProxy(server, bypass string, target *url.URL) (*url.URL, error) {
	host := target.Hostname()
	for _, pattern := range strings.Split(bypass, ";") {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if pattern == "<local>" && !strings.Contains(host, ".") {
			return nil, nil
		}
		if match, _ := filepath.Match(strings.ToLower(pattern), strings.ToLower(host)); match {
			return nil, nil
		}
	}
	selected := ""
	for _, entry := range strings.Split(server, ";") {
		entry = strings.TrimSpace(entry)
		if name, value, ok := strings.Cut(entry, "="); ok {
			if strings.EqualFold(name, target.Scheme) {
				selected = value
				break
			}
			if strings.EqualFold(name, "socks") && selected == "" {
				selected = "socks5://" + value
			}
		} else if entry != "" {
			selected = entry
		}
	}
	if selected == "" {
		return nil, nil
	}
	if !strings.Contains(selected, "://") {
		selected = "auto://" + selected
	}
	p, err := url.Parse(selected)
	if err != nil || p.Hostname() == "" || p.Path != "" || p.RawQuery != "" || p.Fragment != "" {
		return nil, errors.New("invalid system proxy address")
	}
	return p, nil
}

var proxyProtocolCache sync.Map

func dialNetwork(ctx context.Context, network, address string, trace *networkTrace) (net.Conn, error) {
	proxy, err := configuredNetworkProxy(address)
	if err != nil {
		return nil, fmt.Errorf("proxy configuration: %w", err)
	}
	if proxy == nil {
		return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, network, address)
	}
	trace.event("network.proxy", "start", "Using configured proxy "+proxy.Hostname()+":"+proxy.Port()+" for "+address)
	conn, err := dialNetworkProxy(ctx, proxy, address)
	if err != nil {
		return nil, fmt.Errorf("configured proxy: %w", err)
	}
	trace.event("network.proxy", "ok", "Proxy tunnel established to "+address)
	return conn, nil
}

func dialNetworkProxy(ctx context.Context, proxy *url.URL, address string) (net.Conn, error) {
	scheme := strings.ToLower(proxy.Scheme)
	if scheme != "auto" && scheme != "http" && scheme != "https" && scheme != "socks5" && scheme != "socks5h" {
		return nil, errors.New("unsupported proxy protocol")
	}
	port := proxy.Port()
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
		if scheme == "socks5" || scheme == "socks5h" {
			port = "1080"
		}
	}
	endpoint := net.JoinHostPort(proxy.Hostname(), port)
	if scheme == "auto" {
		if cached, ok := proxyProtocolCache.Load(endpoint); ok {
			scheme = cached.(string)
		}
	}
	connect := func() (net.Conn, func(), error) {
		c, err := (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", endpoint)
		if err != nil {
			return nil, nil, err
		}
		deadline := time.Now().Add(5 * time.Second)
		if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
			deadline = dl
		}
		if err := c.SetDeadline(deadline); err != nil {
			c.Close()
			return nil, nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		return c, func() { stop() }, nil
	}
	conn, stop, err := connect()
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if stop != nil {
			stop()
		}
		if !ok && conn != nil {
			_ = conn.Close()
		}
	}()
	if scheme == "auto" {
		// An untyped Windows proxy may be a mixed HTTP/SOCKS listener. Prefer
		// SOCKS remote DNS when it explicitly accepts the SOCKS5 greeting.
		probeDeadline := time.Now().Add(time.Second)
		if dl, has := ctx.Deadline(); has && dl.Before(probeDeadline) {
			probeDeadline = dl
		}
		_ = conn.SetDeadline(probeDeadline)
		err = socksGreeting(conn, proxy)
		if err == nil {
			scheme = "socks5"
			proxyProtocolCache.Store(endpoint, scheme)
		} else {
			stop()
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			conn, stop, err = connect()
			if err != nil {
				return nil, err
			}
			scheme = "http"
		}
		deadline := time.Now().Add(5 * time.Second)
		if dl, has := ctx.Deadline(); has && dl.Before(deadline) {
			deadline = dl
		}
		_ = conn.SetDeadline(deadline)
	} else if scheme == "socks5" || scheme == "socks5h" {
		err = socksGreeting(conn, proxy)
		if err != nil {
			return nil, err
		}
	}
	if scheme == "socks5" || scheme == "socks5h" {
		if err := socksConnect(conn, address); err != nil {
			return nil, err
		}
	} else {
		if scheme == "https" {
			secure := tls.Client(conn, &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
			if err := secure.HandshakeContext(ctx); err != nil {
				return nil, err
			}
			conn = secure
		}
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
		if proxy.User != nil {
			password, _ := proxy.User.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)))
		}
		if err := req.Write(conn); err != nil {
			return nil, err
		}
		reader := bufio.NewReader(conn)
		res, err := http.ReadResponse(reader, req)
		if err != nil {
			return nil, errors.New("proxy did not return a valid CONNECT response")
		}
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("proxy CONNECT rejected (%d)", res.StatusCode)
		}
		if proxy.Scheme == "auto" {
			proxyProtocolCache.Store(endpoint, scheme)
		}
		if reader.Buffered() > 0 {
			conn = &bufferedNetworkConn{Conn: conn, reader: reader}
		}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}

type bufferedNetworkConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedNetworkConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func socksGreeting(conn net.Conn, proxy *url.URL) error {
	methods := []byte{5, 1, 0}
	if proxy.User != nil {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(methods); err != nil {
		return err
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 {
		return errors.New("proxy is not a SOCKS5 listener")
	}
	if reply[1] == 0 {
		return nil
	}
	if reply[1] != 2 || proxy.User == nil {
		return errors.New("SOCKS5 proxy requires unsupported authentication")
	}
	user := proxy.User.Username()
	password, _ := proxy.User.Password()
	if len(user) > 255 || len(password) > 255 {
		return errors.New("SOCKS5 credentials exceed protocol limit")
	}
	auth := append([]byte{1, byte(len(user))}, []byte(user)...)
	auth = append(auth, byte(len(password)))
	auth = append(auth, []byte(password)...)
	if _, err := conn.Write(auth); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 1 || reply[1] != 0 {
		return errors.New("SOCKS5 authentication failed")
	}
	return nil
}

func socksConnect(conn net.Conn, address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	portNumber, err := net.LookupPort("tcp", port)
	if err != nil {
		return err
	}
	if len(host) > 255 {
		return errors.New("SOCKS5 destination hostname is too long")
	}
	// Send the hostname unchanged; DNS resolution belongs to the proxy.
	request := append([]byte{5, 1, 0, 3, byte(len(host))}, []byte(host)...)
	request = append(request, byte(portNumber>>8), byte(portNumber))
	if _, err := conn.Write(request); err != nil {
		return err
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fmt.Errorf("SOCKS5 destination connect failed (status %d)", reply[1])
	}
	length := 0
	switch reply[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return err
		}
		length = int(n[0])
	default:
		return errors.New("invalid SOCKS5 address reply")
	}
	_, err = io.CopyN(io.Discard, conn, int64(length+2))
	return err
}

func networkHTTPTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
	}
	t.TLSClientConfig.Time = discoveryNetworkClock.cachedTime
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialNetwork(ctx, network, address, nil)
	}
	return t
}
