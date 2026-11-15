"""Local test fixture for the documented JSONL key/value discovery contract.

This is not a replacement for the deployed service. It accepts the same
get/set/delete messages, stores opaque strings, and reports epoch expiries.
"""
import argparse
import asyncio
import json
import ssl
import time


class DiscoveryFixture:
    def __init__(self, ttl=600):
        self.ttl = ttl
        self.entries = {}

    def apply(self, request):
        if not isinstance(request, dict):
            return {'ok': False, 'error': 'bad json'}
        op, key = request.get('op'), request.get('key')
        if not isinstance(key, str) or not key:
            return {'ok': False, 'error': 'invalid key'}
        if len(key) > 128:
            return {'ok': False, 'error': 'key too long'}
        if op == 'set':
            value = request.get('value')
            if not isinstance(value, str):
                return {'ok': False, 'error': 'invalid value'}
            if len(value) > 4096:
                return {'ok': False, 'error': 'value too long'}
            expires = time.time() + self.ttl
            self.entries[key] = (value, expires)
            return {'ok': True, 'key': key, 'expires': expires}
        if op == 'get':
            entry = self.entries.get(key)
            if entry is None:
                return {'ok': False, 'error': 'not found'}
            value, expires = entry
            if expires <= time.time():
                del self.entries[key]
                return {'ok': False, 'error': 'expired'}
            return {'ok': True, 'key': key, 'value': value, 'expires': expires}
        if op == 'delete':
            entry = self.entries.pop(key, None)
            return {'ok': True, 'key': key, 'deleted': entry is not None and entry[1] > time.time()}
        return {'ok': False, 'error': 'unknown op'}

    async def handle(self, reader, writer):
        try:
            while True:
                line = await asyncio.wait_for(reader.readline(), 5)
                if not line:
                    break
                if not line.endswith(b'\n') or len(line) > 4096:
                    break
                try:
                    reply = self.apply(json.loads(line))
                except (ValueError, UnicodeError):
                    reply = {'ok': False, 'error': 'bad json'}
                writer.write(json.dumps(reply, separators=(',', ':')).encode() + b'\n')
                await writer.drain()
        except (ConnectionError, asyncio.TimeoutError, ValueError):
            pass
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except (OSError, ConnectionError):
                pass


async def main(args):
    context = None
    if args.cert:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(args.cert, args.key)
    fixture = DiscoveryFixture(args.ttl)
    server = await asyncio.start_server(fixture.handle, '127.0.0.1', args.port, ssl=context, limit=4096)
    print(json.dumps({'host': '127.0.0.1', 'port': server.sockets[0].getsockname()[1]}), flush=True)
    async with server:
        await server.serve_forever()


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=0)
    parser.add_argument('--ttl', type=float, default=600)
    parser.add_argument('--cert')
    parser.add_argument('--key')
    asyncio.run(main(parser.parse_args()))
