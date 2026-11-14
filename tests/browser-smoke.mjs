// No-dependency Chrome DevTools protocol smoke test. Start two headless Chromium
// profiles on ports 9222 and 9223; point each at the same QuietLink hub.
import assert from 'node:assert/strict';

class Browser {
  static async connect(port) {
    const tabs = await (await fetch(`http://127.0.0.1:${port}/json`)).json();
    const tab = tabs.find(x => x.type === 'page' && x.url.includes('8787')) || tabs.find(x => x.type === 'page');
    return new Browser(tab.webSocketDebuggerUrl);
  }
  constructor(url) {
    this.ws = new WebSocket(url); this.seq = 0; this.pending = new Map(); this.consoleErrors = [];
    this.ready = new Promise((resolve, reject) => {
      this.ws.onopen = resolve; this.ws.onerror = reject;
      this.ws.onmessage = ({ data }) => {
        const msg = JSON.parse(data);
        if (msg.id) {
          const [resolve, reject] = this.pending.get(msg.id) || [];
          this.pending.delete(msg.id);
          if (msg.error) reject?.(Error(JSON.stringify(msg.error))); else resolve?.(msg.result);
        } else if (msg.method === 'Runtime.exceptionThrown') {
          this.consoleErrors.push(msg.params.exceptionDetails?.text || 'JS exception');
        }
      };
    });
  }
  async send(method, params = {}) {
    await this.ready;
    const id = ++this.seq;
    return new Promise((resolve, reject) => {
      this.pending.set(id, [resolve, reject]);
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }
  async eval(expression) {
    const resp = await this.send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true, userGesture: true });
    if (resp.exceptionDetails) throw Error(`JS exception: ${JSON.stringify(resp.exceptionDetails)}`);
    return resp.result.value;
  }
  async wait(condition, name, timeout=13000) {
    const started = Date.now();
    while (Date.now() - started < timeout) {
      if (await this.eval(condition)) return;
      await new Promise(r => setTimeout(r, 180));
    }
    throw Error(`Timed out: ${name}; page=${await this.eval(`JSON.stringify({url:document.URL,ready:document.readyState,loaded:typeof callId})`)}`);
  }
  close() { this.ws.close(); }
}
const [a, b] = await Promise.all([Browser.connect(9222), Browser.connect(9223)]);
try {
  await Promise.all([a.send('Runtime.enable'), b.send('Runtime.enable')]);
  const nav = await Promise.all([a.send('Page.navigate', {url:'http://127.0.0.1:8787/'}), b.send('Page.navigate',{url:'http://127.0.0.1:8787/'})]);
  for (const v of nav) if (v.errorText) throw Error(`Cannot navigate to test hub: ${v.errorText}`);
  await new Promise(r=>setTimeout(r,1000));
  await a.wait("typeof callId !== 'undefined' && !!document.querySelector('#nameInput')", 'Alice onboarding');
  await b.wait("typeof callId !== 'undefined' && !!document.querySelector('#nameInput')", 'Bob onboarding');
  await a.eval("document.querySelector('#nameInput').value='Alice';document.querySelector('#onboardingForm').requestSubmit();true");
  await b.eval("document.querySelector('#nameInput').value='Bob';document.querySelector('#onboardingForm').requestSubmit();true");
  await Promise.all([a.wait('hubOnline && !!callId', 'Alice presence'),b.wait('hubOnline && !!callId', 'Bob presence')]);
  const aid=await a.eval('identity.id'), bid=await b.eval('identity.id');
  const bcall=await b.eval('callId');
  console.log('identities and call IDs established', aid, bid);
  await a.eval(`document.querySelector('#friendIdInput').value='${bid}';document.querySelector('#addFriendForm').requestSubmit();true`);
  await b.wait('friendState.incoming.length === 1', 'Bob friend inbox');
  await b.eval('document.querySelector(".request-actions button").click();true');
  await Promise.all([a.wait('friendState.friends.length===1','Alice contact'),b.wait('friendState.friends.length===1','Bob contact')]);
  await a.eval(`ui.dial.value='${bcall}';ui.connect.click();true`);
  await b.wait('!!currentInvite','Bob incoming invitation');
  await b.eval("document.querySelector('#acceptCall').click();true");
  await Promise.all([a.wait("control?.readyState==='open'", 'Alice datachannel',20000),b.wait("control?.readyState==='open'",'Bob datachannel',20000)]);
  console.log('WebRTC P2P data channels connected');
  await a.eval("ui.message.value='Smoke hello from Alice';document.querySelector('#composer').requestSubmit();true");
  await b.wait("ui.history.textContent.includes('Smoke hello from Alice')", 'Bob sees P2P message');
  console.log('message transmitted and rendered');
  const gainCheck = await a.eval("ui.gain.value='42';ui.gain.dispatchEvent(new Event('input'));[ui.gainValue.textContent, gainFactor(), ui.gain.hasAttribute('min'), ui.gain.hasAttribute('max')]");
  assert.equal(gainCheck[0],'42 dB'); assert(Math.abs(gainCheck[1] - 125.89254118)<0.001); assert(!gainCheck[2] && !gainCheck[3]);
  await a.eval("ui.gain.value='300';ui.gain.dispatchEvent(new Event('input'));true");
  assert.equal(await a.eval('gainFactor()'), 1e15);
  await a.eval("ui.gain.value='0';ui.gain.dispatchEvent(new Event('input'));true");
  console.log('uncapped dB gain conversion verified');
  await a.eval("ui.voice.click();true");
  await a.wait('!!media', 'Alice microphone starts');
  await a.eval("ui.voice.click();true");
  await a.wait('!media', 'Alice microphone stops');
  console.log('microphone lifecycle tested');
  await a.eval("offerFile(new File(['QUIETLINK-BYTES-ONLY-AFTER-ACCEPT'], 'p2p-proof.txt', {type:'text/plain'}));true");
  await b.wait("!!document.querySelector('.file-download')", 'file offer arrives');
  const pre = await a.eval("[...sentFiles.values()].map(v=>({sending:v.sending,size:v.file.size}))");
  assert.equal(pre.length,1); assert.equal(pre[0].sending,false); assert(pre[0].size>0);
  assert.equal(await b.eval('receivingFiles.size'),0);
  console.log('file offered without sender reading/sending bytes');
  await b.eval("window.showSaveFilePicker=undefined;document.querySelector('.file-download').click();true");
  await b.wait("ui.history.textContent.includes('downloaded')",'Bob downloaded file',20000);
  await a.wait("ui.history.textContent.includes('received by peer')",'Alice sees transfer confirmation',20000);
  console.log('receiver-triggered P2P file transfer complete');
  const result={alice:aid,bob:bid,aliceHistory:await a.eval('historyFor(identity.id === selectedId ? identity.id : selectedId).then(a=>a.length)'),bobHistory:await b.eval('historyFor(selectedId).then(a=>a.length)'),aliceErrors:a.consoleErrors,bobErrors:b.consoleErrors};
  console.log(JSON.stringify(result,null,2));
  assert.equal(result.aliceErrors.length,0);
  assert.equal(result.bobErrors.length,0);
} finally { a.close(); b.close(); }
