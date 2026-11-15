# Ghost rendezvous directory (Python)

> Legacy binary protocol reference. The desktop client now uses the existing JSONL key/value discovery service over TLS TCP. See [current discovery setup](../DISCOVERY.md). This older two-port binary server is not the service used by the desktop client.

**A public address book, not a communication relay.** The server has exactly two TCP sockets, stores signed 128-byte advertisements **only in RAM**, and expires them after **30 minutes**. No database, accounts, message history, files, or Go/Java/Node runtime.

## Host it

Python 3.10+ and the `cryptography` package are required to verify signed advertisements.

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
python ghost_directory.py --bind 0.0.0.0 --max-memory-mb 16
```

Defaults: TCP **47801** receives publications; TCP **47802** answers lookups. Open these ports in your hosting firewall. Advertisements expire automatically, including when a publisher stops refreshing. A restart clears the entire directory.

For Internet hosting, **TLS is recommended**. Use a real certificate (or terminate both raw TCP ports through a TLS-capable TCP proxy, *not* a normal HTTP reverse proxy):

```bash
python ghost_directory.py --bind 0.0.0.0 \
  --publish-port 47801 --lookup-port 47802 \
  --max-memory-mb 16 \
  --tls-cert /path/fullchain.pem --tls-key /path/privkey.pem
```

The signed data is public. TLS protects privacy against passive observers and makes directory access harder to manipulate. Self-signed server certificates will fail normal Ghost clients unless their CA is trusted.

The included `systemd/ghost-directory.service` is a **non-TLS example**. After creating user `ghostdir`, installing the files and venv under `/opt/ghost-directory`, and adjusting ownership, copy the unit to `/etc/systemd/system`, run `systemctl daemon-reload`, and enable/start it. The unit imposes a **128 MiB process memory cap** via systemd's `MemoryMax`. If you enable TLS, grant the service user permission to read its key/certificate and add the matching arguments.

## Connect Ghost desktop clients

Each independent desktop node still runs its own Go app and peer listener (TCP **8788**). Configure one optional common **directory host**, *not* a common Ghost hub:

```bash
./Ghost-Linux-amd64 \
  -directory-host directory.example.com \
  -directory-tls=true \
  -peer-advertise https://my-node.example.com:8788 \
  -peer-tls-cert /path/node.crt -peer-tls-key /path/node.key
```

To use a simple plaintext directory, omit `-directory-tls=true` on each desktop; signed advertisements still provide content authenticity. Replace `directory.example.com` and `my-node.example.com` with **your own** DNS names. Directory DNS names may be long; **advertised peer hostnames in the 128-byte format are limited to 24 ASCII bytes** (use an IPv4/IPv6 address or short DNS alias if necessary).

The desktop displays a **64-bit directory code** under Nearby nodes when publication succeeds. Share it with a friend. They enter that code in **PEER ADDRESS OR DIRECTORY CODE**. Ghost resolves the signed advertisement, verifies the node identity, then connects **directly to that node**. Known peers are checked for address changes on subsequent reconnects.

The client checks its own entry every 3 minutes and republishes when missing, changed, or older than 8 minutes. Publication is optional: omitting `-peer-advertise` allows lookups without publishing an unusable loopback address.

**Critical networking limitation:** Directory entries only contain **addresses**, not a path through NAT. Peers must advertise **publicly reachable endpoints** and expose TCP 8788, e.g. by port forwarding or hosting on a public IP. If both parties are behind CGNAT/restrictive NAT, the directory alone cannot connect them. WebRTC voice/data may additionally require STUN or a TURN relay. The directory stores neither of those, and never forwards your messages or files.

## Binary protocol

| Socket | Client writes | Server replies |
| --- | --- | --- |
| Publish TCP 47801 | Exactly **128 bytes** signed advertisement | **8 bytes** positive signed 64-bit big-endian code, or zero on rejection |
| Lookup TCP 47802 | Exactly **8 bytes** signed 64-bit big-endian code | Exactly **128 bytes** advertisement, or all zero bytes if unavailable |

128-byte advertisement structure:

```text
bytes 0..31    Ed25519 public key (32 bytes)
bytes 32..63   signed endpoint:
               1 byte version (=1)
               1 byte kind (1=IPv4, 2=IPv6, 3=DNS; bit 7=TLS)
               2 bytes TCP port (big-endian)
               4 bytes Unix time in seconds (big-endian)
               24 bytes IP/DNS, zero padded
bytes 64..127  Ed25519 signature (64 bytes)
               over b"ghost-directory-v1\\0" + bytes 0..63
```

The directory code is the first 63 bits of SHA-256(public key). It remains stable if the address changes or the directory restarts. The server verifies signatures and accepts only announcements signed within ±5 minutes of its clock; it refuses rollback to older entries. Clients verify signatures on lookup. Clock synchronization (NTP) matters.

## Memory and abuse limits

- **No disk writes.** Entries expire in 30 minutes; a server restart erases them.
- `--max-memory-mb` limits the **record store**, using a conservative budget of 1024 bytes per entry (so 16 MiB permits at most 16,384 nodes). The running Python process uses additional memory; enforce a **hard RSS cap** with a systemd cgroup `MemoryMax` if required.
- Publishing is limited to 60 requests/min/IP; lookups to 240/min/IP. Client concurrency and distinct rate-limit IP buckets are bounded.
- On a full store the server rejects new records (returns code 0) without evicting active nodes. Expired entries are swept lazily and do not occupy active slots.
- Anonymous lookups require knowing a random-looking 63-bit node code. This is *not an access-control secret*: anybody with the code may obtain the publicly advertised address. The server does not offer directory-wide enumeration.

## Quick manual test

```bash
python ghost_directory_cli.py --server 127.0.0.1 publish \
  --node-key /path/to/ghost-data.json.nodekey \
  --url http://127.0.0.1:8788

python ghost_directory_cli.py --server 127.0.0.1 lookup YOUR_CODE
```

The CLI accepts `--tls` when the server uses TLS. A local loopback URL is suitable *only for testing*, not internet discovery. **Never upload or disclose your `.nodekey` private file.**

Tests: `python -m unittest discover -s . -p 'test_*.py'`.
