# Ghost

Small, self-hosted Go + browser WebRTC application for **direct voice, chat, and on-demand file transfer**. No Node runtime, npm packages, external database, SMTP server, or cloud account is needed.

## Start on Windows

1. Unzip the Windows release and launch **Ghost.exe** (or `Start Ghost.cmd`). Ghost runs in the notification area (system tray) without a console window. Right-click its icon for **Open Ghost** / **Exit Ghost**. Use **Ghost-console.exe** for verbose logs and headless hosting (`-tray=false -open=false`).
2. The Go hub starts at `http://127.0.0.1:8787` and automatically opens your browser.
3. Choose a display name to create your persistent **Mail ID**. Back up your **identity key** using the button on the left.
4. To add a contact, enter their **Mail ID** under **Add by Mail ID**. They accept your request in **Inbox**. When the contact is online, the friend list shows their **live Call ID**.
5. Select the contact, click **Connect**, and have them accept. Once the direct data channel opens, you can chat, send file offers, or start your mic.

**Important: both people must use the same hub.** Running the app separately on two PCs creates *two independent hubs*, so those IDs cannot see each other. To connect over the internet, host one shared Ghost server using a reachable URL and HTTPS (see below).

## Gain and SNR

- **Microphone gain** is an uncapped *numeric dB* setting, not a percentage. `0 dB` means unity; `+6 dB` roughly doubles amplitude, and `-6 dB` roughly halves it. Use the unrestricted number field or ±3 dB buttons. dBi measures antenna gain, **not microphone amplification**.
- An extreme entry can exceed the browser audio engine's numerical capabilities; those values are rejected instead of silently limited. Large boosts can clip audio and cause loud playback.
- **SNR noise gate** sets the adaptive signal-to-noise *opening threshold* from 0 to 24 dB. It attenuates quiet input, but cannot physically improve a microphone's SNR. This is not a recording-quality/SNR enhancement engine.

## Files: receiver must request the bytes

Click `+` to **offer** a local file. Only name, type, and size are announced through the encrypted data channel. The sender does **not** read or transmit the file contents yet. The receiver must choose **Download** (and, when available, a save destination). Only then does the sender open a separate WebRTC data channel and stream chunks. The hub never receives the file contents.

For large files, Chrome/Edge can save directly to disk via the File System Access API. Browsers without that API support the fallback (maximum 100 MB to avoid keeping huge downloads in RAM). Offered files are tied to the active browser session and must be offered again after disconnect or refresh. Connection interruption can require a fresh offer.

## Built-in inbox, not SMTP

The server maintains Mail IDs, friend requests, and an inbox for accepting/declining them, plus live online presence and Call ID routing. This is simpler and more reliable for a self-hosted application than SMTP, which needs external addresses, credentials, and a mail provider. **Offline chat messages are not sent by mail**: chat and file transfers require both users to establish a WebRTC connection. Chat history is stored in each browser's **IndexedDB**, not the server, so another browser/device will not automatically inherit it.

## Hosting one hub for remote friends

A shared hub provides **identity, friend requests, presence, and WebRTC signaling only**. It is not a media server. On Windows, for example:

```powershell
.\Ghost-console.exe -listen 0.0.0.0:8787 -tls-cert server.crt -tls-key server.key -open=false -tray=false
```

Use a certificate valid for your public DNS name, and connect both browsers to `https://your-domain:8787/`. You can alternatively terminate HTTPS at a properly configured reverse proxy; forward the signaling SSE stream without response buffering. Keep the hub data file private; it contains identity-token hashes and the friend graph (not chat contents).

Default ICE configuration includes public **STUN servers** for direct connections, with **no TURN relay** enabled. Some NATs/firewalls cannot establish a direct WebRTC path. The optional TURN flags support relay fallback but then encrypted packets travel through the relay rather than directly between endpoints:

```powershell
.\Ghost-console.exe -help
```

The hub stores no voice, chat history, or file bytes. WebRTC encrypts active peer connections (DTLS/SRTP); the hub still sees user IDs, online status, and signaling metadata. Protect the exported identity key as a password: anyone holding it can assume that identity. This is a practical personal-use prototype, **not an audited secure messenger**.

## Build from source

Install Go 1.23+ and run:

```bash
go test ./...
go build -o ghost .
```

To cross-compile for Windows:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o Ghost-console.exe .

# Windows desktop executable (no console window)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-H=windowsgui" -o Ghost.exe .
```

No third-party Go packages are required. All web assets are embedded into the executable. Release builds embed the Windows icon resource. Keep `ghost.ico` beside the executable as a tray-icon fallback.

Existing v1 browser identities use the original `quietlink.*` localStorage keys for compatibility. If upgrading from QuietLink, rename `quietlink-data.json` to `ghost-data.json` or pass `-data quietlink-data.json`.

## Windows tray and icons

The bundled `ghost.ico`, multi-resolution PNGs, and `ghost.svg` provide a consistent identity across Explorer/taskbar, Windows notification tray, and the web favicon. The tray has Open Ghost and Exit Ghost commands. The optional `-tray=false` switch disables it. Server mode does not require a graphical desktop.

## Validation scope

- Go tests exercise registration, authentication, friend-request inbox, persistence, live Call ID discovery, signaling, and rejected cross-origin requests. `go test -race ./...` passes.
- JavaScript passes `node --check web/app.js` syntax validation.
- A two-browser smoke-test script is provided in `tests/browser-smoke.mjs`. It is designed to verify chat, mic, and receiver-requested file transfer; **it has not passed in the build environment**, where managed Chromium blocks navigation to localhost (`ERR_BLOCKED_BY_ADMINISTRATOR`). It can be run on a machine where Chromium allows local access.

## Notes

This is v1. Friends must be online for live messages and file transfers. Local history and saved identities stay in the browser used to create them. Back up your access key before clearing browser storage. A central hub is required even though media/file content is peer-to-peer.