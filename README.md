# Ghost

**Peer-to-peer voice, chat, and file sharing.** Ghost is a small Go-hosted WebRTC messenger with a quiet dark interface, an animated ghost mascot, a friend-request inbox, and local message history.

[**Download for Windows**](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.1/Ghost-Windows-amd64.exe) · [**Download for Linux**](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.1/Ghost-Linux-amd64) · [**Android preview**](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.0/Ghost-Android-v1.1.0-preview.apk) · [**All releases**](https://github.com/Cryosimorgh/Ghost/releases)

> **Current desktop release: v1.1.1.** Windows and Linux downloads are self-contained native executables: **no Java, Node.js, npm, or Go installation** is needed. They host the UI locally in your **existing web browser**. The Android build is an experimental client, not a standalone hub.

## What it does

| | |
| --- | --- |
| **Voice** | Direct WebRTC audio with microphone selection, selectable output where supported, unrestricted numeric gain in **dB**, and an adjustable 0–24 dB SNR noise gate |
| **Mic preview** | Local microphone test, live frequency spectrum, and input level display |
| **Messaging** | WebRTC data-channel messages with per-device history in browser IndexedDB |
| **Files** | Offer a file by metadata; its bytes remain with the sender **until the recipient chooses Download** |
| **Contacts** | Persistent Mail ID, friend requests and inbox, online presence, and session-specific Call IDs |
| **Interface** | Responsive People / Chat / Audio views, animated ghost with pointer-tracking eyes, and reduced-motion support |
| **Desktop** | Single native Go executable with embedded frontend, Windows tray controls, and Linux desktop browser launching |

The built-in inbox manages friend requests; it is **not SMTP or an offline message-delivery system**.

## Install and run

### Windows (x64)

1. Download [**Ghost-Windows-amd64.exe**](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.1/Ghost-Windows-amd64.exe).
2. Run it. Ghost starts the local hub at `http://127.0.0.1:8787` and opens your browser.
3. Find Ghost in the notification area (system tray) for **Open** and **Exit**.

No installer or additional runtime is necessary. The [Windows ZIP](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.1/Ghost-Windows-v1.1.1.zip) also contains a console executable for troubleshooting or server operation.

### Linux (x86-64)

Download [**Ghost-Linux-amd64**](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.1/Ghost-Linux-amd64), then:

```bash
chmod +x Ghost-Linux-amd64
./Ghost-Linux-amd64
```

With a graphical desktop, Ghost opens the system browser. The executable is built with `CGO_ENABLED=0` and does not require Java, Node, or Go. A [Linux ZIP](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.1/Ghost-Linux-v1.1.1.zip) with a launcher is also available.

For a headless server:

```bash
./Ghost-Linux-amd64 -open=false -listen 0.0.0.0:8787
```

**Remote access needs HTTPS** to use microphones in typical browsers. See [Host a shared hub](#host-a-shared-hub) before exposing a server to the internet.

### Android (experimental)

The [v1.1.0 Android preview APK](https://github.com/Cryosimorgh/Ghost/releases/download/v1.1.0/Ghost-Android-v1.1.0-preview.apk) is a **WebView client** for Android 10+; it does not run a Go hub on the phone.

1. Set up a reachable Ghost hub with a valid HTTPS certificate.
2. Install and open the APK, then enter the hub's `https://` address.
3. Grant microphone access when prompted to test your mic or start voice.

The Android client includes a system file picker and a native Downloads integration. Audio device choices depend on Android version and WebView support. It is a **debug-signed preview**, not a production-distributed or fully validated mobile app; its signature may differ between builds, requiring uninstall/reinstall. No background calling or push notifications.

## Connect to a friend

1. Open Ghost and create a name. Your **Mail ID** stays associated with your account; your **Call ID** changes between live sessions.
2. Back up your **identity key** using **BACKUP KEY**. Possession of this key grants access to your Ghost identity.
3. Add a friend's Mail ID under **People**. They accept your request in **Inbox**.
4. When they are online **on the same hub**, select them and choose **Connect** (or dial their live Call ID). The other person accepts.
5. Send messages, offer files with **+**, or select **Start voice**.

**Important:** Running Ghost on two computers independently starts **two different hubs**. They cannot discover each other's Mail IDs by default. To communicate across devices or over the internet, both must open the **same shared Ghost hub URL**.

## Audio controls

Open **Audio** to choose a microphone, select a playback device if the browser supports it, and click **Test mic** to see a local spectrum without transmitting audio.

- **Gain** is an unrestricted numeric **dB** entry (`0 dB` = unity, `+6 dB` ≈ 2× amplitude). There is no artificial UI cap; extreme values may clip, be rejected by the audio engine, or damage hearing at high playback levels. **dBi** is for antenna gain, not microphones.
- **SNR noise gate** ranges from 0 to 24 dB. It controls when quiet input is attenuated; it **does not improve the physical microphone SNR**.
- **Audio output selection** uses browser `setSinkId` where available. Otherwise select output in the operating system. Android may route audio through its native controls.

## How files work

1. **Offer:** the sender advertises the filename, MIME type, and size only.
2. **Request:** the receiver clicks **Download**.
3. **Transfer:** only then does the sender stream the file bytes over a separate WebRTC data channel.

The signaling hub does **not** receive file contents. Transfers require both peers online with an active connection; interruptions may require re-offering the file. Chrome/Edge can stream larger downloads directly to disk when the File System Access API is available. Browser fallback downloads are limited to **100 MB** to avoid excessive memory use. Android can save received files under Downloads/Ghost.

## Host a shared hub

Ghost's Go process handles **identity, friend requests, presence, and WebRTC signaling**, not hosted voice/chat/file storage. You need one shared instance for friends on different machines.

Example with a valid TLS certificate for your hostname:

```bash
./Ghost-Linux-amd64 -listen 0.0.0.0:8787 -open=false \
  -tls-cert server.crt -tls-key server.key
```

Then both users visit `https://your-domain:8787/`. Alternatively terminate HTTPS at a reverse proxy configured to forward signaling events without buffering. Protect the server's `ghost-data.json` file and identity credentials.

Ghost uses public **STUN** servers by default and no TURN relay. Some NAT/firewall combinations block direct WebRTC connectivity. Optional `-turn-url`, `-turn-user`, and `-turn-pass` flags support relay fallback, meaning encrypted traffic can travel **through a relay** rather than on a direct network path.

```bash
./Ghost-Linux-amd64 -help
```

## Data and security

- The server stores account metadata, token hashes, and friend relationships in `ghost-data.json` (or the path supplied by `-data`).
- Chat history is stored **locally in the current browser's IndexedDB**. It does not automatically sync between desktop browsers and Android devices.
- Voice and chat/file connections use WebRTC encryption. The hub still sees identity, presence, and signaling metadata.
- Message and file contents are **not** uploaded to the hub. Offline messages are not queued for delivery.
- Export and safeguard your identity key before clearing browser data.
- Ghost is a **personal-use prototype**, not an independently audited secure messenger.

## Build from source

Requires **Go 1.23+** for the desktop app; no third-party Go modules or Node runtime are required to build/run the Go server:

```bash
go test ./...
go build -o ghost .
./ghost
```

The frontend is embedded with Go's `embed` package. The [GitHub Actions workflow](https://github.com/Cryosimorgh/Ghost/blob/main/.github/workflows/release.yml) cross-compiles native Windows/Linux releases, adds Windows icon resources, and separately compiles the Android debug APK with **JDK 17, Android SDK, and Gradle**. Node is used in CI only to syntax-check the browser script.

To build Android locally:

```bash
gradle -p android :app:assembleDebug
```

The Android output is `android/app/build/outputs/apk/debug/app-debug.apk`.

## Testing and limitations

- Automated Go tests cover registration, authentication, friend requests, presence, persistence, signaling, and access checks. Windows/Linux and Android build jobs have passed in GitHub Actions.
- JavaScript syntax checking passes. A two-browser WebRTC smoke test is included at `tests/browser-smoke.mjs`, but **live end-to-end voice, chat, and file transfer remain unverified in CI** due to Chromium localhost restrictions.
- Browser microphone and output-device APIs vary; HTTPS is generally required for microphone access on a remote host.
- No background Android calls, push notifications, offline chat delivery, or cross-device history synchronization.

[**Releases**](https://github.com/Cryosimorgh/Ghost/releases) · [**Issues**](https://github.com/Cryosimorgh/Ghost/issues) · [**Source**](https://github.com/Cryosimorgh/Ghost)
