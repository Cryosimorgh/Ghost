import asyncio
import hashlib
import ipaddress
import struct
import time
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat

from ghost_directory import Directory, Store, DOMAIN, record_id_and_timestamp, ZERO_RECORD


def make_record(key: Ed25519PrivateKey, host='203.0.113.11', port=8788, timestamp=None):
    pub = key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    address = ipaddress.ip_address(host)
    endpoint = struct.pack('!BBHI', 1, 0x81 if address.version == 4 else 0x82, port, int(time.time()) if timestamp is None else timestamp)
    endpoint += address.packed.ljust(24, b'\0')
    signature = key.sign(DOMAIN + pub + endpoint)
    assert len(pub + endpoint + signature) == 128
    return pub + endpoint + signature


class ProtocolTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory = Directory(Store(max_memory_mb=1, ttl_seconds=2))
        self.publisher = await asyncio.start_server(self.directory.publish, '127.0.0.1', 0)
        self.resolver = await asyncio.start_server(self.directory.lookup, '127.0.0.1', 0)
        self.publish_port = self.publisher.sockets[0].getsockname()[1]
        self.lookup_port = self.resolver.sockets[0].getsockname()[1]
        self.key = Ed25519PrivateKey.generate()

    async def asyncTearDown(self):
        self.publisher.close()
        self.resolver.close()
        await self.publisher.wait_closed()
        await self.resolver.wait_closed()

    async def request(self, port, payload, expected):
        reader, writer = await asyncio.open_connection('127.0.0.1', port)
        writer.write(payload)
        await writer.drain()
        result = await asyncio.wait_for(reader.readexactly(expected), 3)
        writer.close()
        await writer.wait_closed()
        return result

    async def test_publish_lookup_refresh_and_expire(self):
        one = make_record(self.key)
        reply = await self.request(self.publish_port, one, 8)
        number = struct.unpack('!q', reply)[0]
        self.assertGreater(number, 0)
        self.assertEqual(await self.request(self.lookup_port, reply, 128), one)
        self.assertEqual(await self.request(self.publish_port, one, 8), reply)
        two = make_record(self.key, host='203.0.113.12', timestamp=int(time.time()) + 1)
        self.assertEqual(await self.request(self.publish_port, two, 8), reply)
        self.assertEqual(await self.request(self.lookup_port, reply, 128), two)
        # An old, still correctly signed advertisement cannot overwrite the newer one.
        self.assertEqual(await self.request(self.publish_port, one, 8), bytes(8))
        self.directory.store.entries[number].expires_at = time.monotonic() - 1
        self.assertEqual(await self.request(self.lookup_port, reply, 128), ZERO_RECORD)

    async def test_reject_tampering_future_and_old_signatures(self):
        good = make_record(self.key)
        tampered = bytearray(good)
        tampered[42] ^= 1
        self.assertEqual(await self.request(self.publish_port, bytes(tampered), 8), bytes(8))
        old = make_record(self.key, timestamp=int(time.time()) - 301)
        self.assertEqual(await self.request(self.publish_port, old, 8), bytes(8))
        future = make_record(self.key, timestamp=int(time.time()) + 301)
        self.assertEqual(await self.request(self.publish_port, future, 8), bytes(8))
        self.assertGreater(struct.unpack('!q', await self.request(self.publish_port, good, 8))[0], 0)

    async def test_memory_cap_not_bypassable(self):
        self.directory.store.max_records = 2
        first, second, third = [make_record(Ed25519PrivateKey.generate()) for _ in range(3)]
        self.assertNotEqual(await self.request(self.publish_port, first, 8), bytes(8))
        self.assertNotEqual(await self.request(self.publish_port, second, 8), bytes(8))
        self.assertEqual(await self.request(self.publish_port, third, 8), bytes(8))
        self.assertEqual(len(self.directory.store.entries), 2)

    async def test_short_and_unknown_lookup(self):
        self.assertEqual(await self.request(self.lookup_port, struct.pack('!q', 123), 128), ZERO_RECORD)
        reader, writer = await asyncio.open_connection('127.0.0.1', self.publish_port)
        writer.write(b'x' * 8)
        writer.close()
        await writer.wait_closed()
        await asyncio.sleep(0.01)
        self.assertEqual(self.directory.active, 0)


if __name__ == '__main__':
    unittest.main()
