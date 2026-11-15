#!/usr/bin/env python3
"""Test the Ghost directory with a real node key (or resolve a 64-bit code)."""
import argparse
import base64
import hashlib
import ipaddress
import socket
import ssl
import struct
import time

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from cryptography.exceptions import InvalidSignature
from urllib.parse import urlsplit
from ghost_directory import DOMAIN, ZERO_RECORD


def connect(host, port, tls):
    conn = socket.create_connection((host, port), timeout=6)
    conn.settimeout(6)
    if tls:
        try:
            conn = ssl.create_default_context().wrap_socket(conn, server_hostname=host)
        except Exception:
            conn.close()
            raise
    return conn


def read_exact(conn, size):
    result = bytearray()
    while len(result) < size:
        chunk = conn.recv(size - len(result))
        if not chunk:
            raise EOFError('directory closed connection before replying')
        result.extend(chunk)
    return bytes(result)


def make_record(key_path, raw_url):
    encoded = open(key_path, encoding='ascii').read().strip()
    private_bytes = base64.b64decode(encoded, validate=True)
    private = Ed25519PrivateKey.from_private_bytes(private_bytes[:32])
    pub = private.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    url = urlsplit(raw_url)
    if url.scheme not in ('http', 'https') or not url.hostname or url.path or url.query or url.fragment or url.username:
        raise ValueError('expected http(s)://hostname:port with no path')
    port = url.port or (443 if url.scheme == 'https' else 80)
    try:
        ip = ipaddress.ip_address(url.hostname)
        kind = 1 if ip.version == 4 else 2
        host = ip.packed
    except ValueError:
        kind = 3
        host = url.hostname.lower().encode('ascii')
        if len(host) > 24 or not all(c in b'abcdefghijklmnopqrstuvwxyz0123456789.-' for c in host):
            raise ValueError('DNS hostname must be up to 24 ASCII bytes')
    if url.scheme == 'https':
        kind |= 0x80
    endpoint = struct.pack('!BBHI', 1, kind, port, int(time.time())) + host.ljust(24, b'\0')
    signed = pub + endpoint
    return signed + private.sign(DOMAIN + signed)


def explain(record, code):
    if record == ZERO_RECORD:
        return 'Not found (expired, missing, or directory restarted)'
    pub, endpoint, signature = record[:32], record[32:64], record[64:]
    node_id = int.from_bytes(hashlib.sha256(pub).digest()[:8], 'big') & ((1 << 63) - 1)
    if code != node_id:
        raise ValueError('incorrect node ID for this signed record')
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(signature, DOMAIN + record[:64])
    except InvalidSignature:
        raise ValueError('signature verification failed')
    version, kind, port, timestamp = struct.unpack('!BBHI', endpoint[:8])
    if version != 1:
        raise ValueError('unsupported version')
    host = endpoint[8:]
    if kind & 0x7f == 1:
        address = str(ipaddress.IPv4Address(host[:4]))
    elif kind & 0x7f == 2:
        address = '[' + str(ipaddress.IPv6Address(host[:16])) + ']'
    elif kind & 0x7f == 3:
        address = host.rstrip(b'\0').decode('ascii')
    else:
        raise ValueError('invalid address type')
    scheme = 'https' if kind & 0x80 else 'http'
    return f'URL: {scheme}://{address}:{port}\nNode key: {base64.urlsafe_b64encode(pub).rstrip(b"=").decode()}\nSigned at: {time.strftime("%Y-%m-%d %H:%M:%S UTC", time.gmtime(timestamp))}'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', required=True, help='directory DNS hostname or IP (no scheme)')
    parser.add_argument('--tls', action='store_true', help='verify TLS certificate on both sockets')
    parser.add_argument('--publish-port', type=int, default=47801)
    parser.add_argument('--lookup-port', type=int, default=47802)
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('publish')
    p.add_argument('--node-key', required=True, help='path to Ghost .nodekey; NEVER share this file')
    p.add_argument('--url', required=True, help='reachable HTTP(S) peer endpoint')
    q = sub.add_parser('lookup')
    q.add_argument('code', type=int)
    args = parser.parse_args()
    if args.command == 'publish':
        record = make_record(args.node_key, args.url)
        with connect(args.server, args.publish_port, args.tls) as conn:
            conn.sendall(record)
            code = struct.unpack('!q', read_exact(conn, 8))[0]
        if code == 0:
            raise SystemExit('Directory rejected record (capacity, signature or clock error)')
        print(f'Published as {code} (expires in 30 minutes without refresh)')
    else:
        if args.code <= 0 or args.code > (1 << 63) - 1:
            parser.error('ID must be positive signed int64')
        with connect(args.server, args.lookup_port, args.tls) as conn:
            conn.sendall(struct.pack('!q', args.code))
            record = read_exact(conn, 128)
        print(explain(record, args.code))


if __name__ == '__main__':
    main()
