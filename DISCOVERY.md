# Discovery over TLS TCP

For the deployed discovery service, use:

| Setting | Value |
| --- | --- |
| Discovery server address | `www.soulless.ai/v/api/ghost` (or `www.soulless.ai`) |
| Discovery TCP port | `443` |
| Use TLS | Checked |
| TLS server name / SNI | `www.soulless.ai` (derived from the hostname) |

This connection uses **TCP → verified TLS → newline-delimited JSON**. It sends no HTTP request, URL path, HTTP headers, health check, or ping. The TLS checkbox controls whether the socket is wrapped in TLS; checked TLS never falls back to plaintext. Certificate and hostname checks use the default trust store.

The address label `/v/api/ghost` is accepted in the settings field and removed before dialing. The TCP destination and TLS SNI remain `www.soulless.ai` on port `443`.

The local browser interface and the separate peer signaling listener have their own transports. The directory TLS setting controls the discovery socket only.

## Service contract

Each request and reply is one JSON object followed by a newline:

```json
{"op":"set","key":"room1","value":"opaque string"}
{"ok":true,"key":"room1","expires":1794770000}
{"op":"get","key":"room1"}
{"ok":true,"key":"room1","value":"opaque string","expires":1794770000}
{"op":"delete","key":"room1"}
{"ok":true,"key":"room1","deleted":true}
```

Missing or expired keys return `{"ok":false,"error":"not found"}` or `{"ok":false,"error":"expired"}`. Keys and values are exact, case-sensitive strings. The store does not verify advertisements or interpret endpoints. The server controls TTL; a `set` refreshes the lease, while a `get` does not.

## Ghost's peer discovery

Ghost derives its stable decimal directory code from the node public key, then uses the exact key `ghost:v1:<decimal-code>`. Both publication and lookup use that same namespace and decimal encoding.

The stored value is an unpadded base64url encoding of Ghost's signed 128-byte endpoint advertisement. The server stores it as an opaque string. Ghost verifies the signature, directory code, timestamp, and endpoint before contacting the peer. A connected peer must prove the same node identity. Advertised DNS names in this record format remain limited to 24 ASCII bytes.

Ghost writes its advertisement with `set`, reads it back with `get`, then refreshes the lease at half the server-reported remaining TTL, with a maximum interval of three minutes. This keeps short leases alive. Refresh failure is logged and retried after 15 seconds.

**Test directory connection** uses `get` with a random probe key. `not found` is a valid service response. With a public peer URL, the test additionally publishes the local node's signed advertisement and verifies its readback. It does not send an invalid binary advertisement, a ping, or an HTTP request.

Set a reachable **Public peer URL**, save/restart, and share the directory code. The other node resolves it and connects directly to the peer. Discovery carries no signaling, messages, files, or media. Reachable peer signaling and WebRTC connectivity are checked separately.

## Diagnostics and tests

**Settings → Connection diagnostics** shows destination host/port, transport, TLS/SNI/certificate verification, JSONL operation and framing, server errors, advertisement verification, peer handshake, signaling, ICE, and data-channel states. JSON values, credentials, SDP, candidate payloads, message bodies, and file bytes are omitted.

The log is copyable and also saved as `<data-file>.network.log`, rotating at 2 MiB with one backup. The panel shows the actual log path.

```bash
go test ./...
go test -race ./...
go vet ./...
python -m unittest discover -s tests -p 'test_*.py' -v
node tests/discovery-browser.mjs
```

The browser test needs Playwright/Chromium. The Python JSONL fixture uses only the standard library and models the documented deployed contract; it does not replace the deployed server. Set `GHOST_TEST_PYTHON` to require the Python integration tests rather than allowing a skip when Python is unavailable.

An optional **read-only live check** uses exactly the deployed hostname and port, verifies TLS/SNI, and sends one JSONL `get` for a random probe key:

```bash
GHOST_LIVE_DISCOVERY=1 go test -run '^TestLiveDiscoveryTLSGet$' -v -count=1
```

The older binary server under `directory/` is retained as a historical protocol reference. The desktop client uses the JSONL service described here.
