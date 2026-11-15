#!/usr/bin/env python3
"""Ghost's two-port, volatile, authenticated rendezvous directory.

Publish socket: 128-byte Ed25519-signed advertisement -> signed int64 (8 bytes).
Lookup socket: signed int64 (8 bytes) -> advertisement (128 bytes), or all zeroes.
All integers use network byte order. Nothing is written to disk.
"""
from __future__ import annotations

import argparse
import asyncio
from dataclasses import dataclass
import hashlib
import ipaddress
import logging
import ssl
import struct
import time

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

RECORD_BYTES = 128
ID_BYTES = 8
DOMAIN = b"ghost-directory-v1\0"
ZERO_RECORD = bytes(RECORD_BYTES)
MAX_AGE = 300  # Signatures must be fresh to prevent indefinite replay.


@dataclass(slots=True)
class Entry:
    record: bytes
    signed_at: int
    expires_at: float


def record_id_and_timestamp(record: bytes, now: int | None = None) -> tuple[int, int]:
    """Check signature, timestamp and endpoint encoding, then return stable node ID."""
    if len(record) != RECORD_BYTES:
        raise ValueError("record must be exactly 128 bytes")
    pub, endpoint, signature = record[:32], record[32:64], record[64:]
    version, kind, port, timestamp = struct.unpack("!BBHI", endpoint[:8])
    host = endpoint[8:]
    if version != 1 or kind not in (1, 2, 3, 0x81, 0x82, 0x83) or not port:
        raise ValueError("invalid endpoint format")
    kind_no_tls = kind & 0x7f
    try:
        if kind_no_tls == 1:
            if host[4:] != bytes(20):
                raise ValueError("nonzero padding")
            ipaddress.IPv4Address(host[:4])
        elif kind_no_tls == 2:
            if host[16:] != bytes(8):
                raise ValueError("nonzero padding")
            ipaddress.IPv6Address(host[:16])
        else:
            name = host.rstrip(b"\0")
            if not name or host != name.ljust(24, b"\0") or not all(x in b"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-" for x in name) or name.startswith(b"-") or name.endswith(b"-"):
                raise ValueError("invalid DNS name")
    except ipaddress.AddressValueError as exc:
        raise ValueError("invalid IP") from exc
    now = int(time.time()) if now is None else now
    if abs(now - timestamp) > MAX_AGE:
        raise ValueError("expired or future-dated signature")
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(signature, DOMAIN + record[:64])
    except (InvalidSignature, ValueError) as exc:
        raise ValueError("invalid signature") from exc
    node_id = int.from_bytes(hashlib.sha256(pub).digest()[:8], "big") & ((1 << 63) - 1)
    if node_id == 0:
        raise ValueError("reserved ID")
    return node_id, timestamp


class Store:
    """Bounded, RAM-only records. The cap is a conservative per-record budget."""

    def __init__(self, max_memory_mb: int = 16, ttl_seconds: int = 1800):
        if not 1 <= max_memory_mb <= 1024 or not 1 <= ttl_seconds <= 1800:
            raise ValueError("invalid memory or TTL")
        # Allow 1024 bytes per record for Python dict+entry overhead (~128-byte payload).
        self.max_records = max(1, max_memory_mb * 1024 * 1024 // 1024)
        self.ttl_seconds = ttl_seconds
        self.entries: dict[int, Entry] = {}
        self._last_sweep = 0.0

    def sweep(self, now: float | None = None, force: bool = False) -> None:
        now = time.monotonic() if now is None else now
        if force or now - self._last_sweep >= 30:
            expired = [key for key, item in self.entries.items() if item.expires_at <= now]
            for key in expired:
                self.entries.pop(key, None)
            self._last_sweep = now

    def publish(self, record: bytes) -> int:
        try:
            node_id, signed_at = record_id_and_timestamp(record)
        except ValueError:
            return 0
        now = time.monotonic()
        self.sweep(now, force=len(self.entries) >= self.max_records)
        previous = self.entries.get(node_id)
        if previous and previous.record[:32] != record[:32]:
            return 0  # Reject a different public key even if its 63-bit ID collided.
        if previous and signed_at < previous.signed_at:
            return 0  # Never roll back to an older signed endpoint.
        if previous and signed_at == previous.signed_at and record != previous.record:
            return 0
        if previous is None and len(self.entries) >= self.max_records:
            return 0
        self.entries[node_id] = Entry(record, signed_at, now + self.ttl_seconds)
        return node_id

    def lookup(self, node_id: int) -> bytes:
        now = time.monotonic()
        self.sweep(now)
        entry = self.entries.get(node_id)
        if entry is None:
            return ZERO_RECORD
        if entry.expires_at <= now:
            del self.entries[node_id]
            return ZERO_RECORD
        return entry.record


class RateLimiter:
    def __init__(self, limit_per_minute: int, max_ips: int = 4096):
        self.limit = limit_per_minute
        self.max_ips = max_ips
        self.counts: dict[str, tuple[int, int]] = {}

    def allowed(self, ip: str) -> bool:
        now = int(time.monotonic() // 60)
        old = self.counts.get(ip)
        if old is None and len(self.counts) >= self.max_ips:
            self.counts = {k: v for k, v in self.counts.items() if v[0] >= now - 1}
            if len(self.counts) >= self.max_ips:
                return False
        count = old[1] if old and old[0] == now else 0
        self.counts[ip] = (now, count + 1)
        return count < self.limit


class Directory:
    def __init__(self, store: Store, max_clients: int = 128):
        self.store = store
        self.max_clients = max_clients
        self.active = 0
        self.publish_limit = RateLimiter(60)
        self.lookup_limit = RateLimiter(240)

    async def handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter, publish: bool) -> None:
        try:
            ip = writer.get_extra_info("peername")[0]
            limit = self.publish_limit if publish else self.lookup_limit
            if self.active >= self.max_clients or not limit.allowed(ip):
                return
            self.active += 1
            try:
                request = await asyncio.wait_for(reader.readexactly(128 if publish else 8), timeout=5)
                if publish:
                    response = struct.pack("!q", self.store.publish(request))
                else:
                    node_id = struct.unpack("!q", request)[0]
                    response = self.store.lookup(node_id)
                writer.write(response)
                await asyncio.wait_for(writer.drain(), timeout=5)
            finally:
                self.active -= 1
        except (asyncio.IncompleteReadError, asyncio.TimeoutError, ConnectionError):
            pass
        except Exception:
            logging.exception("client processing failed")
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except (ConnectionError, OSError):
                pass

    async def publish(self, r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
        await self.handle(r, w, True)

    async def lookup(self, r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
        await self.handle(r, w, False)


async def serve(args: argparse.Namespace) -> None:
    context = None
    if bool(args.tls_cert) != bool(args.tls_key):
        raise SystemExit("--tls-cert and --tls-key must be provided together")
    if args.tls_cert:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(args.tls_cert, args.tls_key)
    directory = Directory(Store(args.max_memory_mb), args.max_clients)
    publisher = await asyncio.start_server(directory.publish, args.bind, args.publish_port, ssl=context, backlog=64, limit=256)
    resolver = await asyncio.start_server(directory.lookup, args.bind, args.lookup_port, ssl=context, backlog=64, limit=256)
    logging.info("Ghost directory: publish %s:%s, lookup %s:%s, TTL 30m, RAM budget %s MiB, record cap %s, TLS %s", args.bind, args.publish_port, args.bind, args.lookup_port, args.max_memory_mb, directory.store.max_records, bool(context))
    async with publisher, resolver:
        await asyncio.gather(publisher.serve_forever(), resolver.serve_forever())


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bind", default="0.0.0.0")
    parser.add_argument("--publish-port", type=int, default=47801)
    parser.add_argument("--lookup-port", type=int, default=47802)
    parser.add_argument("--max-memory-mb", type=int, default=16, help="conservative RAM-only record budget, 1-1024 MiB (not process RSS)")
    parser.add_argument("--max-clients", type=int, default=128)
    parser.add_argument("--tls-cert", help="PEM certificate; enable TLS on both ports")
    parser.add_argument("--tls-key", help="PEM private key")
    args = parser.parse_args()
    if args.publish_port == args.lookup_port or args.max_clients < 1:
        parser.error("ports must differ and max-clients must be positive")
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    try:
        asyncio.run(serve(args))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
