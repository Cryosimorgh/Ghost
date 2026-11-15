import asyncio
import json
import time
import unittest
from unittest.mock import patch

from discovery_fixture import DiscoveryFixture


class StoreContractTests(unittest.TestCase):
    def test_values_and_case_are_verbatim(self):
        store = DiscoveryFixture()
        value = '  opaque\n2001:db8::1 ☃ '
        store.apply({'op': 'set', 'key': 'Foo', 'value': value})
        self.assertEqual(store.apply({'op': 'get', 'key': 'Foo'})['value'], value)
        self.assertEqual(store.apply({'op': 'get', 'key': 'foo'})['error'], 'not found')

    def test_ttl_set_refreshes_and_get_does_not(self):
        store = DiscoveryFixture(ttl=10)
        with patch('discovery_fixture.time.time', return_value=100):
            self.assertEqual(store.apply({'op': 'set', 'key': 'room', 'value': 'one'})['expires'], 110)
        with patch('discovery_fixture.time.time', return_value=105):
            self.assertEqual(store.apply({'op': 'get', 'key': 'room'})['expires'], 110)
            self.assertEqual(store.apply({'op': 'set', 'key': 'room', 'value': 'two'})['expires'], 115)
        with patch('discovery_fixture.time.time', return_value=116):
            self.assertEqual(store.apply({'op': 'get', 'key': 'room'})['error'], 'expired')

    def test_delete_and_validation(self):
        store = DiscoveryFixture()
        store.apply({'op': 'set', 'key': 'room', 'value': 'one'})
        self.assertTrue(store.apply({'op': 'delete', 'key': 'room'})['deleted'])
        self.assertFalse(store.apply({'op': 'delete', 'key': 'room'})['deleted'])
        for req in [{'op': 'ping', 'key': 'x'}, {'op': 'get', 'key': ''}, {'op': 'get', 'key': 'x'*129}, {'op': 'set', 'key': 'x', 'value': 'x'*4097}]:
            self.assertFalse(store.apply(req)['ok'])


class JSONLContractTests(unittest.IsolatedAsyncioTestCase):
    async def test_fragmented_lines_and_many_ops_on_one_connection(self):
        fixture = DiscoveryFixture()
        server = await asyncio.start_server(fixture.handle, '127.0.0.1', 0, limit=4096)
        async with server:
            reader, writer = await asyncio.open_connection('127.0.0.1', server.sockets[0].getsockname()[1])
            for part in [b'{"op":"set","key":"room",', b'"value":"opaque"}\n{"op":"get","key":"room"}\n']:
                writer.write(part)
                await writer.drain()
            self.assertTrue(json.loads(await reader.readline())['ok'])
            self.assertEqual(json.loads(await reader.readline())['value'], 'opaque')
            writer.write(b'bad json\n')
            await writer.drain()
            self.assertEqual(json.loads(await reader.readline()), {'ok': False, 'error': 'bad json'})
            writer.close()
            await writer.wait_closed()


if __name__ == '__main__':
    unittest.main()
