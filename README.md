# Ghost

**Voice, chat, and receiver-approved file sharing — peer to peer, without a Ghost-operated server.**

Ghost is a lightweight Go desktop application with a local web UI, an animated ghost, contacts, WebRTC calls, messages, and local history. **Each desktop instance runs its own node**; two users do not have to connect to the same hub.

[**Windows EXE**](https://github.com/Cryosimorgh/Ghost/releases/download/v1.4.0/Ghost-Windows-amd64.exe) · [**Linux binary**](https://github.com/Cryosimorgh/Ghost/releases/download/v1.4.0/Ghost-Linux-amd64) · [**Releases**](https://github.com/Cryosimorgh/Ghost/releases) · [**Issues**](https://github.com/Cryosimorgh/Ghost/issues)

> **Scope of v1.4.0:** Automatic LAN discovery, an **optional self-hosted RAM-only Python rendezvous directory** for finding nodes by stable 64-bit codes, and a **separate one-time Direct Connect funnel** for pairing by an encrypted invite code without using a directory. There is no global account search, public server run by Ghost, NAT relay, or guaranteed cross-NAT connectivity. The directory is *not* the shared Ghost signaling hub from early releases. The Android v1.1 preview is still a WebView client of a reachable desktop node, **not a fully standalone decentralized Android node**. Live two-browser WebRTC media/data transfer has not yet been independently verified.

## Quick start

**Windows (x64):** download [Ghost-Windows-amd64.exe](https://github.com/Cryosimorgh/Ghost/releases/download/v1.4.0/Ghost-Windows-amd64.exe), run it, and allow local-network access through Windows Firewall when prompted. Ghost runs in the notification tray and opens your browser automatically.

**Linux (x86-64):** download [Ghost-Linux-amd64](https://github.com/Cryosimorgh/Ghost/releases/download/v1.4.0/Ghost-Linux-amd64):

```bash
chmod +x Ghost-Linux-amd64
./Ghost-Linux-amd64
```

Both are single Go executables with the UI embedded. **No Java, Node, npm, Go installation, or hosted signaling account is required.** They use an existing browser to display the UI. Ghost's browser interface is local to your device at `http://127.0.0.1:8787`; network peers use a **separate** listener on TCP `8788`.

### Find another Ghost on your LAN

1. Both computers launch Ghost. Use the same LAN, with multicast UDP **47383** and peer TCP **8788** allowed through their firewalls.
2. Open **People → Nearby nodes**. Discovered instances appear automatically through multicast announcements; no central directory or public server is queried.
3. Click **+ name** to request friendship, or manually enter the friend's Mail ID. The other user accepts the request from **Inbox**.
4. Online friends expose their live Call IDs. Click **Connect**, then use voice, chat, or receiver-triggered file downloads.

If multicast is blocked, copy **your node address** from the Nearby nodes panel and have your friend paste it into **Direct peer address**. Use `http://192.168.x.y:8788` or the actual address shown. A successful connection exchanges each node's public key and online user information and is saved for reconnection.

### Direct Connect: one-use invite code (v1.4, no directory)

**This is a separate, optional connection route.** LAN discovery, saved peer addresses, and the Python directory remain unchanged.

1. **Host** opens **People → ONE-TIME DIRECT LINK**. Enter the public IP or DNS name the other person can reach, without a scheme or port. On a publicly reachable host, leave **TCP port = 0** to use an operating-system-assigned ephemeral port. Behind a router, choose a fixed TCP port and manually forward that same port to the host computer. Permit it through the OS firewall.
2. Click **CREATE INVITE**. Ghost opens **only a temporary, one-use TCP listener** on that pairing port and creates a copyable `GHOST1.…` code containing an encrypted endpoint, the permanent node public key, a random secret, and an expiry. The invite expires after **10 minutes**, or immediately on successful connection or Revocation.
3. **Friend** opens Ghost, pastes the complete code into **CONNECT WITH A CODE**, and clicks **CONNECT DIRECTLY**. They do **not** need the directory address or an HTTP peer URL.
4. The two Ghost instances perform a PSK-authenticated **ephemeral X25519 handshake**, verify the invited host's Ed25519 key, and establish a persistent **AES-GCM encrypted bidirectional TCP signaling tunnel**. The host then **closes the listening socket** while the established tunnel stays active. Both users appear under Nearby nodes and can send friend requests and begin normal calls. The tunnel uses **one established TCP connection** for signaling in both directions.

**What the code does and does not hide:** It is a high-entropy *bearer invitation*; the encrypted payload is opaque to someone without the code, but its decryption key is itself inside the code so it can work with no third-party service. **Anyone holding the whole code can recover the host IP and attempt the one-time connection.** Share privately; it is **not anonymity**. The code expires locally and is not stored on a rendezvous server.

**Port details:** `0` requests an ephemeral (OS-selected) port, **not automatic NAT mapping**. If you need router port forwarding, choose a **fixed** port so the external and internal port match. The pairing socket closes after the first authenticated session; existing Ghost's TCP `8788` listener is **not** disabled or changed by this feature. The established tunnel persists only while both applications remain connected; after a disconnect, generate a new code. A dynamic public IP requires a fresh code when it changes.

**Network reality:** At least **one computer must be reachable** on its selected TCP pairing port. If neither has a publicly reachable IP/forwarded port (e.g. both behind restrictive CGNAT), the invite cannot magically connect them. The encrypted control tunnel handles **signaling**, not WebRTC media packets. Audio/chat/files still use their existing WebRTC path and may need STUN/TURN or suitable NAT mapping. No relay, public bootstrap host, or internet IP lookup is introduced by Direct Connect.

### Find a remote peer by directory code (v1.3)

Host your own short-lived Python directory (two TCP ports, 128-byte signed ads, 30-minute expiry; **no chat/file storage**) using [directory setup](directory/README.md). Each client chooses whether to opt in; there is no built-in public server address.

```bash
# Run on your own VPS (after pip install -r directory/requirements.txt):
python directory/ghost_directory.py --bind 0.0.0.0 --max-memory-mb 16

# On a desktop with a publicly reachable peer listener:
./Ghost-Linux-amd64 -directory-host your.directory.example \
  -peer-advertise http://198.51.100.40:8788
```

For public deployment, use `--tls-cert ... --tls-key ...` on the **directory** and `-directory-tls=true` on Ghost; configure peer TLS separately with `-peer-tls-cert`/`-peer-tls-key` and a matching `https://` peer address.

**Share the 64-bit code shown under Nearby nodes**. Your friend enters it in **PEER ADDRESS OR DIRECTORY CODE**. Ghost validates the signed advertisement and the contacted peer's identity before saving the connection. It will automatically look up known peers again when saved addresses become unreachable. The publisher checks the directory every 3 minutes and re-advertises when needed; records expire 30 minutes after the last successful publication.

The directory only helps peers **find publicly reachable node addresses**. One side must have an inbound reachable node (or there must be additional NAT traversal infrastructure). **Two CGNAT-bound computers may still fail**. Directory lookup is neither STUN nor a TURN relay.

### Connect across the internet

There is **no Ghost-run public server or TURN relay**. You can exchange a one-use Direct Connect code, share a reachable peer address manually, or optionally host a tiny rendezvous directory and share a 64-bit code. Nodes pin the peer's public key when first reached. The receiving node must have a port forwarded or a public address; only **one node must accept inbound peer signaling**. The other can maintain a signed, outbound polling connection and receive friend requests, acceptances, and call signals through it. With both nodes behind restrictive NATs or CGNAT, the initial connection may still be impossible without a reachable third peer or infrastructure. WebRTC media traversal is a separate concern.

For a reachable host with a TLS certificate, run e.g.:

```bash
./Ghost-Linux-amd64 -peer-listen 0.0.0.0:8788 \
  -peer-advertise https://ghost.example.com:8788 \
  -peer-tls-cert server.crt -peer-tls-key server.key
```

The peer port is **8788**, not the local browser port **8787**. Expose only the peer port, keep the browser hub on loopback, and use TLS for internet-facing mesh traffic. A valid certificate is required for HTTPS; otherwise the peer protocol uses plain HTTP and its signaling/identity metadata is visible to network intermediaries (WebRTC message/media content is separately encrypted).

To opt into external STUN services for NAT traversal, pass `-stun stun:your-stun-server:3478`. **No public STUN server is configured by default**. Optional `-turn-url`, `-turn-user`, and `-turn-pass` are supported but necessarily introduce a relay; that is not strictly direct P2P.

## Architecture

```text
Ghost A desktop                          Ghost B desktop
+------------------------+               +------------------------+
| Browser UI (localhost) |               | Browser UI (localhost) |
| Go node + mailbox      | <-signed P2P->| Go node + mailbox      |
| UDP LAN announcement   |     TCP       | UDP LAN announcement   |
+------------------------+               +------------------------+
           \_______________________________/
              WebRTC audio / chat / files
              (only after user acceptance)
```

- **Discovery:** optional encrypted one-time Direct Connect codes, UDP multicast on the LAN, saved node addresses or manually entered direct URLs for other networks. There is no DHT or default public directory; private/self-hosted directory lookup is optional.
- **Signaling:** existing direct node-to-node HTTP(S) signed requests **or** the new authenticated, encrypted full-duplex TCP tunnel. Both use persistent Ed25519 node keys, freshness checks, and replay prevention. A peer's node key is retained across restarts in `ghost-data.json.nodekey` (private, do not share it).
- **Friends:** each node owns its local identities/inbox and stores a pinned remote-node key for a remote contact. Friend requests and acceptances travel directly between online nodes, not through SMTP or a common hub.
- **Calling:** each browser session gets a temporary Call ID. Ghost finds that ID on a known live peer node and sends WebRTC offers, answers, and ICE candidates directly. Browser-to-browser voice/messages/files use WebRTC rather than the peer signaling connection.
- **Storage:** account metadata and known peer URLs live in local `ghost-data.json`; chat history is saved in that browser's IndexedDB. No automatic history sync or offline message delivery.

### File transfer is receiver-initiated

The sender advertises only file metadata. **The sender reads/transmits file bytes only after the recipient clicks Download.** A separate WebRTC data channel carries file chunks, not the Go signaling mesh. In compatible Chrome/Edge versions, direct-to-disk streaming supports larger files; fallback is limited to 100 MB. Android preview has native Downloads integration. Interruptions may require a fresh file offer.

## Controls

Under **Audio**: select microphone and (when supported) playback device; preview the mic's frequency spectrum; set unrestricted numeric **gain in dB** (`0` dB = unity), and an SNR **noise-gate threshold** from 0–24 dB. A noise gate does *not* increase the physical microphone SNR. Extreme amplification can clip or damage hearing. dBi measures antenna gain, not microphones.

The UI has desktop and mobile-responsive layouts, reduced-motion support, and a small animated ghost with eye tracking and idle behavior. Windows uses a tray icon; Linux opens the default browser on graphical desktops.

## Security and limitations

- P2P **node identity** is Ed25519-signed. The first encounter with a node is **trust-on-first-use**, not an independently authenticated identity; verify the node's key out of band if identity spoofing matters. User Mail IDs are still locally generated and **not cryptographically derived from a per-user public key**.
- The LAN hello endpoint advertises active local Mail IDs, names, and Call IDs to reachable nodes. Disable discovery (`-lan=false`) or restrict access with a firewall when that is undesirable.
- On the legacy direct HTTP route, node-to-node signaling metadata is visible unless TLS is enabled. The one-use Direct Connect route encrypts signaling over the tunnel independently of HTTP TLS. WebRTC media/data channels use DTLS/SRTP encryption. Remote addresses may reveal IP locations. Signed polling responses support a private node behind NAT when one side has a reachable peer port.
- Browser ICE may fail even when node-to-node signaling succeeds. No default STUN, TURN, NAT punch-through service, or global DHT is provided.
- No offline mail, background call wakeup, push notification, or multi-device browser history sync. Android v1.1.0 is **not** an independent Ghost P2P node and cannot participate in the new automatic LAN discovery on its own.
- Ghost is an **experimental personal-use messenger**, not an audited end-to-end identity or anonymity system.

## CLI options

```text
-listen          Local browser UI address (default 127.0.0.1:8787)
-peer-listen     LAN/internet peer signaling listener (default 0.0.0.0:8788)
-peer-advertise  Public reachable URL for your peer listener (required to publish)
-directory-host    Optional self-hosted Python directory hostname
-directory-publish-port / -directory-lookup-port  Directory TCP ports (47801/47802)
-directory-tls     Verify TLS on directory sockets (default false; recommended publicly)
-peers           Enable direct peer mesh (default true)
-lan             Enable automatic LAN multicast discovery (default true)
-stun            Optional comma-separated STUN URLs (empty by default)
-turn-url        Optional TURN relay URL(s)
-data            Local identity/friend/peer-address database
-peer-tls-cert/key  TLS certificate/key for peer listener only
-tls-cert/key    Optional TLS certificate/key for browser UI only
-open            Open web browser (default true on desktop)
-tray            Windows system tray control
```

## Build and tests

Go 1.23+ is required to **build from source**, not to run a release binary:

```bash
go test ./...
go test -race ./...
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o Ghost-Linux-amd64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags='-H=windowsgui' -o Ghost-Windows-amd64.exe .
```

`directory/test_directory.py` verifies the signed, 128-byte, 30-minute, RAM-only TCP protocol. `directory_client_test.go` verifies Ghost client encoding and two independent nodes using a live Python directory. `direct_test.go` covers encrypted invite tamper/expiry, socket closure, independent nodes, two-way friend acceptance, and signaling over a single encrypted tunnel. `mesh_test.go` runs **two independent Go nodes with separate identity databases** and verifies mutual peer discovery by URL, cross-node friend requests/acceptance, online Call IDs, signaling in both directions, signed-message replay prevention, and node-key persistence. Optional real multicast test: `GHOST_TEST_LAN=1 go test -run TestLANMulticastDiscovery -v` (requires working multicast interface). Live WebRTC browser tests remain unverified in the existing sandbox.

[Source](https://github.com/Cryosimorgh/Ghost) · [Releases](https://github.com/Cryosimorgh/Ghost/releases) · [Bug reports](https://github.com/Cryosimorgh/Ghost/issues)
