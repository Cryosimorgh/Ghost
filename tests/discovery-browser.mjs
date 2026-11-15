// Real Python discovery -> two independent Go nodes -> browser WebRTC.
// Prerequisites: Go, Python (stdlib only), Playwright + Chromium.
// Run: GHOST_TEST_PYTHON=python node tests/discovery-browser.mjs
// GHOST_PLAYWRIGHT_MODULE / GHOST_BROWSER_EXECUTABLE support existing installs.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { once } from 'node:events';
import { createWriteStream, mkdtempSync, mkdirSync } from 'node:fs';
import { createServer } from 'node:net';
import { createSocket } from 'node:dgram';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createInterface } from 'node:readline';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.GHOST_PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const work = mkdtempSync(path.join(tmpdir(), 'ghost-discovery-e2e-'));
const evidence = process.env.GHOST_QA_OUTPUT || path.join(work, 'evidence');
mkdirSync(evidence, { recursive: true });
const python = process.env.GHOST_TEST_PYTHON || 'python';
const children = [];
let browser;
let ntpFixture;

async function freePort() {
  const server = createServer();
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}
function launch(command, args, name, env = process.env) {
  const child = spawn(command, args, { cwd: root, env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
  children.push(child);
  const stream = createWriteStream(path.join(work, `${name}.log`));
  child.stdout.pipe(stream, { end: false });
  child.stderr.pipe(stream, { end: false });
  child.on('close', () => stream.end());
  return child;
}
async function stop(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = once(child, 'exit');
  child.kill();
  await exited;
}
async function until(fn, label, ms = 15000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try { if (await fn()) return; } catch {}
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw Error(`Timed out: ${label}; process logs in ${work}`);
}
// Poll through CDP evaluation so the test also works with the app's strict CSP
// on older Playwright versions, without enabling unsafe-eval in Ghost.
async function waitFor(page, fn, arg, ms = 15000) {
  return until(() => page.evaluate(fn, arg), fn.toString(), ms);
}

try {
  const binary = path.join(work, process.platform === 'win32' ? 'Ghost.exe' : 'ghost');
  execFileSync('go', ['build', '-o', binary, '.'], { cwd: root, stdio: 'inherit', windowsHide: true });
  const directory = launch(python, ['-u', 'tests/discovery_fixture.py'], 'directory');
  const lines = createInterface({ input: directory.stdout });
  const [line] = await Promise.race([once(lines, 'line'), new Promise((_, reject) => setTimeout(() => reject(Error('Python directory did not start')), 10000).unref())]);
  lines.close();
  const discovery = JSON.parse(line);
  ntpFixture=createSocket('udp4');
  ntpFixture.on('message',(request,remote)=>{
    if(request.length<48)return;
    const reply=Buffer.alloc(48);reply[0]=0x24;reply[1]=2;request.copy(reply,24,40,48);
    const seconds=Date.now()/1000+2208988800;
    for(const offset of [32,40]){reply.writeUInt32BE(Math.floor(seconds)>>>0,offset);reply.writeUInt32BE(Math.floor((seconds%1)*2**32)>>>0,offset+4);}
    ntpFixture.send(reply,remote.port,remote.address);
  });
  await new Promise(resolve=>ntpFixture.bind(0,'127.0.0.1',resolve));
  const ntpAddress=`127.0.0.1:${ntpFixture.address().port}`;
  const urls = [];
  for (const name of ['alice', 'bob']) {
    const uiPort = await freePort(), peerPort = await freePort();
    const configDir = path.join(work, `${name}-config`);
    mkdirSync(configDir);
    launch(binary, ['-data', path.join(work, `${name}-data.json`), '-listen', `127.0.0.1:${uiPort}`,
      '-peer-listen', `127.0.0.1:${peerPort}`, '-peer-advertise', `http://127.0.0.1:${peerPort}`,
      '-directory-host', discovery.host, '-directory-port', String(discovery.port), '-directory-tls=false',
      '-ntp-server',ntpAddress,
      '-lan=false', '-stun=', '-turn-url=', '-open=false', '-tray=false'], name,
    { ...process.env, APPDATA: configDir, XDG_CONFIG_HOME: configDir });
    const url = `http://127.0.0.1:${uiPort}`;
    urls.push(url);
    await until(async () => (await fetch(`${url}/api/config`)).ok, `${name} local node ready`);
  }
  browser = await chromium.launch({
    headless: true,
    ...(process.env.GHOST_BROWSER_EXECUTABLE ? { executablePath: process.env.GHOST_BROWSER_EXECUTABLE } : {}),
    args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
  const errors = [];
  const contexts = await Promise.all(urls.map(() => browser.newContext({ viewport: { width: 1440, height: 1000 }, permissions: ['microphone', 'clipboard-read', 'clipboard-write'] })));
  const pages = await Promise.all(contexts.map(context => context.newPage()));
  for (let i = 0; i < pages.length; i++) {
    const page = pages[i];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', msg => {
      // Deliberately invalid discovery settings below must return HTTP 400 locally.
      if (msg.type() === 'error' && !msg.location().url.endsWith('/api/settings/test-directory')) errors.push(msg.text());
    });
    await page.goto(urls[i]);
    assert.match(await page.title(), /Ghost/);
    await page.locator('#nameInput').fill(i ? 'Bob' : 'Alice');
    await page.locator('#onboardingForm').evaluate(form => form.requestSubmit());
    await waitFor(page, () => hubOnline && !!callId);
    assert.equal(await page.locator('.wordmark').textContent(), 'ghost');
  }
  const [a, b] = pages;
  await a.locator('.primary-nav [data-view="settings"]').click();
  await waitFor(a, host => document.querySelector('#cfgDirectoryHost').value === host, discovery.host);
  assert.equal(await a.locator('#cfgDirectoryPublishPort').isDisabled(), false);
  assert.equal(await a.locator('#cfgDirectoryLookupPort').count(), 0, 'Only one discovery TCP port is exposed');
  await a.locator('#testDirectory').click();
  await waitFor(a, () => document.querySelector('#directoryTestStatus').textContent.includes('Signed publication and lookup verified'));
  await waitFor(a, () => document.querySelector('#networkLogOutput').textContent.includes('directory.test'));
  await a.locator('#copyNetworkLogs').click();
  await until(() => a.evaluate(async () => (await navigator.clipboard.readText()).includes('directory.test')), 'copy diagnostics');
  await a.locator('#cfgDirectoryTLS').check();
  await a.locator('#cfgDirectoryHost').fill('http://www.soulless.ai/v/api/ghost');
  assert.equal(await a.locator('#cfgDirectoryTLS').isChecked(), true, 'Typing a URL cannot disable TLS');
  assert.equal(await a.locator('#cfgDirectoryTLS').isDisabled(), false);
  await a.locator('#testDirectory').click();
  await waitFor(a, () => document.querySelector('#directoryTestStatus').textContent.includes('do not enter an HTTP URL'));
  await waitFor(a, () => document.querySelector('#networkLogOutput').textContent.includes('[ERROR] directory.config'));
  await a.locator('#networkLogOutput').scrollIntoViewIfNeeded();
  await a.screenshot({ path: path.join(evidence, 'discovery-diagnostics-desktop.png') });
  await a.setViewportSize({ width: 390, height: 844 });
  await a.locator('#networkLogOutput').scrollIntoViewIfNeeded();
  assert(await a.locator('#networkLogOutput').isVisible());
  assert(await a.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  await a.screenshot({ path: path.join(evidence, 'discovery-diagnostics-mobile.png') });
  await a.setViewportSize({ width: 1440, height: 1000 });
  await a.locator('#cfgDirectoryHost').fill(discovery.host);
  await a.locator('#cfgDirectoryTLS').uncheck();

  // Only the decimal code is exchanged. LAN and direct invite routes are off.
  await waitFor(b, async () => /^\d+$/.test((await api('/api/peers')).directoryCode));
  const code = await b.evaluate(async () => (await api('/api/peers')).directoryCode);
  await a.locator('.primary-nav [data-view="connect"]').click();
  await a.locator('#meshAddress').fill(code);
  await a.locator('#meshConnectForm').evaluate(form => form.requestSubmit());
  await waitFor(a, async () => (await api('/api/peers')).peers.some(p => p.online));
  await waitFor(b, async () => (await api('/api/peers')).peers.some(p => p.online));
  console.log('PASS: Python discovery, signed peer identity, reciprocal signaling');
  await stop(directory);

  const bobID = await b.evaluate(() => identity.id);
  const bobCall = await b.evaluate(() => callId);
  await a.locator('#friendIdInput').fill(bobID);
  await a.locator('#addFriendForm').evaluate(form => form.requestSubmit());
  await waitFor(b, () => friendState.incoming.length === 1);
  await b.locator('.request-actions button').first().click();
  await Promise.all(pages.map(page => waitFor(page, () => friendState.friends.length === 1)));
  await a.locator('.primary-nav [data-view="chat"]').click();
  await a.locator('#dialInput').fill(bobCall);
  await a.locator('#connectBtn').click();
  await b.locator('#acceptCall').click();
  await Promise.all(pages.map(page => waitFor(page, () => control?.readyState === 'open', null, 30000)));
  for (const [sender, receiver, message] of [[a,b,'Alice over direct WebRTC'], [b,a,'Bob over direct WebRTC']]) {
    await sender.locator('#messageInput').fill(message);
    await sender.locator('#composer').evaluate(form => form.requestSubmit());
    await waitFor(receiver, text => document.querySelector('#history').textContent.includes(text), message);
  }
  console.log('PASS: directory stopped; friend acceptance, WebRTC, and messages work in both directions');
  await a.locator('#voiceBtn').click();
  await waitFor(a, () => !!media);
  await waitFor(b, () => document.querySelector('#remoteAudio').srcObject?.getAudioTracks().some(t => t.readyState === 'live'));
  await a.locator('#voiceBtn').click();
  await waitFor(a, () => !media);
  console.log('PASS: microphone starts and stops; live remote audio track received');

  await a.evaluate(() => offerFile(new File(['GHOST-DISCOVERY-P2P-PROOF'], 'discovery-proof.txt', { type: 'text/plain' })));
  await b.locator('.file-download').waitFor();
  assert.equal(await a.evaluate(() => [...sentFiles.values()][0].sending), false);
  assert.equal(await b.evaluate(() => receivingFiles.size), 0);
  await b.evaluate(() => { window.showSaveFilePicker = undefined; });
  const downloaded = b.waitForEvent('download');
  await b.locator('.file-download').click();
  const download = await downloaded;
  const chunks = [];
  for await (const chunk of await download.createReadStream()) chunks.push(chunk);
  assert.equal(Buffer.concat(chunks).toString(), 'GHOST-DISCOVERY-P2P-PROOF');
  await waitFor(a, () => document.querySelector('#history').textContent.includes('received by peer'));
  console.log('PASS: receiver-triggered file transfer, exact bytes verified');

  const logs = await a.evaluate(() => api('/api/network/logs'));
  assert(logs.events.some(e => e.stage === 'peer.connect' && e.status === 'ok'));
  assert(logs.events.some(e => e.stage === 'webrtc.data' && e.detail === 'open'));
  assert(!JSON.stringify(logs).includes('GHOST-DISCOVERY-P2P-PROOF'));
  assert(!JSON.stringify(logs).includes('Alice over direct WebRTC'));
  assert.deepEqual(errors, [], 'Unexpected browser console/runtime errors');
  await a.screenshot({ path: path.join(evidence, 'discovery-p2p-success.png') });
  console.log(`PASS: no unexpected browser errors; evidence: ${evidence}; process logs: ${work}`);
} finally {
  if (browser) {
    for (const context of browser.contexts()) await context.close();
    await browser.close();
  }
  for (const child of children.reverse()) await stop(child);
  if(ntpFixture)ntpFixture.close();
}
