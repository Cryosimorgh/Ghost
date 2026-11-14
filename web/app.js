'use strict';

const $ = (id) => document.getElementById(id);
const ui = {
  selfName: $('selfName'), selfAvatar: $('selfAvatar'), selfStatus: $('selfStatus'), mailId: $('mailId'),
  callId: $('callId'), presenceDot: $('presenceDot'), hubState: $('hubState'),
  friends: $('friends'), friendCount: $('friendCount'), inbox: $('inbox'), inboxCount: $('inboxCount'),
  conversationTitle: $('conversationTitle'), headerMode: $('headerMode'), connectionDot: $('connectionDot'),
  connectionLabel: $('connectionLabel'), dial: $('dialInput'), history: $('history'),
  connect: $('connectBtn'), voice: $('voiceBtn'), hangup: $('hangupBtn'),
  attach: $('attachBtn'), send: $('sendBtn'), message: $('messageInput'), fileInput: $('fileInput'),
  iceState: $('iceState'), dataState: $('dataState'), voiceState: $('voiceState'), micState: $('micState'),
  levelFill: $('levelFill'), gain: $('gainInput'), snr: $('snrSlider'), gainValue: $('gainValue'), snrValue: $('snrValue'),
  incoming: $('incomingCall'), incomingName: $('incomingName'), incomingText: $('incomingText'),
  remoteAudio: $('remoteAudio'), toast: $('toast'),
  inputDevice: $('inputDevice'), outputDevice: $('outputDevice'), previewMic: $('previewMic'),
  spectrum: $('micSpectrum'), spectrumStatus: $('spectrumStatus'), outputSupport: $('outputSupport')
};
let identity = null, localDB = null, iceServers = [], streamAbort = null, sessionId = crypto.randomUUID();
let callId = '', hubOnline = false, friendState = { friends: [], incoming: [], outgoing: [] };
let selectedId = null, selectedName = '', pc = null, peer = null, control = null, currentInvite = null, outgoingInvite = null;
let queuedICE = [], media = null, previewMedia = null, activeCallSerial = 0;
let meterFrame = 0, deviceRefreshing = false, selectedInput = '', selectedOutput = '';
const sentFiles = new Map(), receivingFiles = new Map(), availableFiles = new Map();
let sseRestart = null, toastTimer = null, renderVersion = 0, inviteTimeout = null;

const sleep = (ms) => new Promise(resolve => setTimeout(resolve, ms));
const makeID = () => crypto.randomUUID();
const bytes = (size) => {
  if (!Number.isFinite(size) || size < 0) return 'Unknown size';
  if (size < 1024) return `${size} B`;
  const units = ['KB', 'MB', 'GB', 'TB']; let i = -1;
  do { size /= 1024; i++; } while (size >= 1024 && i < units.length - 1);
  return `${size.toFixed(size >= 10 ? 0 : 1)} ${units[i]}`;
};
const formatTime = (t) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
function toast(message, error = false) {
  clearTimeout(toastTimer); ui.toast.textContent = message; ui.toast.className = `toast show${error ? ' error' : ''}`;
  toastTimer = setTimeout(() => ui.toast.className = 'toast', 4200);
}
function node(tag, classes, value) {
  const el = document.createElement(tag);
  if (classes) el.className = classes;
  if (value !== undefined) el.textContent = value;
  return el;
}
function setText(id, value) { $(id).textContent = value; }
function setRangePaint(input) {
  const pct = ((Number(input.value) - Number(input.min)) / (Number(input.max) - Number(input.min))) * 100;
  input.style.background = `linear-gradient(to right, #9bd3c1 0%, #9bd3c1 ${pct}%, #364b4b ${pct}%, #364b4b 100%)`;
}
function gainFactor() {
  const db = Number(ui.gain.value);
  if (ui.gain.value.trim() === '' || !Number.isFinite(db)) throw Error('Enter a finite gain in dB');
  const factor = Math.pow(10, db / 20);
  // Web Audio stores gain as a finite float32 AudioParam; this is not a UI-defined cap.
  if (!Number.isFinite(factor) || factor > 3.4028234663852886e38) throw Error('Gain exceeds the Web Audio engine numeric range');
  return factor;
}
function applyGain() {
  try {
    const factor = gainFactor();
    ui.gain.removeAttribute('aria-invalid');
    ui.gainValue.textContent = `${Number(ui.gain.value)} dB`;
    localStorage.setItem('quietlink.gainDb', ui.gain.value);
    if (media) media.gain.gain.setTargetAtTime(factor, media.ctx.currentTime, .02);
  } catch (error) {
    ui.gain.setAttribute('aria-invalid', 'true');
    ui.gainValue.textContent = 'Invalid';
    if (ui.gain.value.trim()) toast(error.message, true);
  }
}
function initControls() {
  const savedGain = localStorage.getItem('quietlink.gainDb');
  ui.gain.value = savedGain !== null && Number.isFinite(Number(savedGain)) ? savedGain : '0';
  const savedGate = Number(localStorage.getItem('quietlink.gate') ?? 6);
  ui.snr.value = Number.isFinite(savedGate) ? Math.max(0, Math.min(24, savedGate)) : 6;
  applyGain(); updateSliders();
  ui.gain.addEventListener('input', applyGain);
  for (const [id, step] of [['gainDown', -3], ['gainUp', 3]]) {
    $(id).addEventListener('click', () => {
      const db = Number(ui.gain.value);
      ui.gain.value = String((Number.isFinite(db) ? db : 0) + step);
      applyGain();
    });
  }
  ui.snr.addEventListener('input', () => { updateSliders(); localStorage.setItem('quietlink.gate', ui.snr.value); media?.gate?.port.postMessage({ thresholdDb: Number(ui.snr.value) }); });
}
function updateSliders() { ui.snrValue.textContent = `${ui.snr.value} dB`; setRangePaint(ui.snr); }

function selectedAudioConstraint() {
  return { echoCancellation: true, noiseSuppression: true, autoGainControl: false,
    ...(selectedInput ? { deviceId: { exact: selectedInput } } : {}) };
}
function fillDevices(select, entries, chosen, defaultLabel) {
  select.replaceChildren();
  const def = document.createElement('option'); def.value = ''; def.textContent = defaultLabel; select.append(def);
  for (const [index, device] of entries.entries()) {
    if (!device.deviceId || device.deviceId === 'default' || device.deviceId === 'communications') continue;
    const option = document.createElement('option'); option.value = device.deviceId;
    option.textContent = device.label || `${device.kind === 'audioinput' ? 'Microphone' : 'Speaker'} ${index + 1}`;
    select.append(option);
  }
  select.value = chosen;
  if (select.value !== chosen) select.value = '';
}
async function refreshAudioDevices() {
  if (deviceRefreshing || !navigator.mediaDevices?.enumerateDevices) return;
  deviceRefreshing = true;
  try {
    const devices = await navigator.mediaDevices.enumerateDevices();
    fillDevices(ui.inputDevice, devices.filter(d => d.kind === 'audioinput'), selectedInput, 'Default microphone');
    const outputDevices = devices.filter(d => d.kind === 'audiooutput');
    fillDevices(ui.outputDevice, outputDevices, selectedOutput, 'Default speakers');
    const supportsSink = typeof ui.remoteAudio.setSinkId === 'function';
    let nativeRoutes = [];
    if (!supportsSink && window.GhostAndroid?.listAudioRoutes) {
      try { nativeRoutes = JSON.parse(window.GhostAndroid.listAudioRoutes()); } catch (e) { console.warn('Android routes:',e); }
      fillDevices(ui.outputDevice, nativeRoutes.map(d=>({deviceId:d.id,label:d.label,kind:'audiooutput'})), selectedOutput, 'System default');
    }
    ui.outputDevice.disabled = !(supportsSink ? outputDevices.length > 0 : nativeRoutes.length > 0);
    ui.outputSupport.textContent = supportsSink ? 'Per-device' : nativeRoutes.length ? 'Android route' : 'System controlled';
  } catch (error) { console.warn('Audio devices:', error); }
  finally { deviceRefreshing = false; }
}
async function applyOutput() {
  try {
    if (typeof ui.remoteAudio.setSinkId === 'function') await ui.remoteAudio.setSinkId(selectedOutput);
    else if (window.GhostAndroid?.setAudioRoute) {
      if (!window.GhostAndroid.setAudioRoute(selectedOutput)) throw Error('Android route unavailable');
    } else return;
    localStorage.setItem('ghost.outputDevice', selectedOutput);
  } catch (e) { toast(`Cannot switch playback: ${e.message}`, true); await refreshAudioDevices(); }
}
function wireAudioDevices() {
  selectedInput = localStorage.getItem('ghost.inputDevice') || '';
  selectedOutput = localStorage.getItem('ghost.outputDevice') || '';
  $('refreshDevices').addEventListener('click', refreshAudioDevices);
  ui.inputDevice.addEventListener('change', async () => {
    selectedInput = ui.inputDevice.value; localStorage.setItem('ghost.inputDevice', selectedInput);
    try {
      if (media) { await disableMic(); await enableMic(); }
      else if (previewMedia) { await stopPreviewMic(); await startPreviewMic(); }
    } catch (e) { toast(`Microphone switch failed: ${e.message}`, true); }
  });
  ui.outputDevice.addEventListener('change', async () => {
    selectedOutput = ui.outputDevice.value; await applyOutput();
  });
  ui.previewMic.addEventListener('click', async () => {
    ui.previewMic.disabled = true;
    try { if (previewMedia) await stopPreviewMic(); else await startPreviewMic(); }
    catch (e) { toast(`Mic test: ${e.message}`, true); }
    finally { ui.previewMic.disabled = false; }
  });
  if (navigator.mediaDevices?.addEventListener) navigator.mediaDevices.addEventListener('devicechange', refreshAudioDevices);
  refreshAudioDevices().then(() => { if (selectedOutput) applyOutput(); });
}

function stopSpectrum() { if (meterFrame) cancelAnimationFrame(meterFrame); meterFrame = 0; }
function updateMicDisplay() {
  const m = media || previewMedia;
  ui.micState.textContent = media ? 'TRANSMITTING' : previewMedia ? 'LOCAL TEST' : 'MIC OFF';
  ui.previewMic.textContent = previewMedia ? 'Stop test' : 'Test mic';
  ui.previewMic.disabled = !!media;
  if (!m) { ui.levelFill.style.width = '0'; ui.spectrumStatus.textContent = 'Waiting for input'; drawSpectrum(null); }
  else ui.spectrumStatus.textContent = media ? 'On air' : 'Local only';
}
function drawSpectrum(m) {
  const canvas = ui.spectrum;
  const ctx = canvas.getContext('2d'); if (!ctx) return;
  const dpr = Math.min(2, window.devicePixelRatio || 1);
  const w = Math.max(220, canvas.clientWidth || 260), h = Math.max(100, canvas.clientHeight || 130);
  if (canvas.width !== Math.round(w*dpr) || canvas.height !== Math.round(h*dpr)) {
    canvas.width = Math.round(w*dpr); canvas.height = Math.round(h*dpr);
  }
  ctx.setTransform(dpr,0,0,dpr,0,0);
  ctx.clearRect(0,0,w,h);
  ctx.lineWidth = 1;
  ctx.strokeStyle = 'rgba(141,169,160,.12)';
  for (let i=1;i<4;i++) { const y = Math.round(h*i/4)+.5; ctx.beginPath(); ctx.moveTo(0,y); ctx.lineTo(w,y); ctx.stroke(); }
  const count = Math.max(28, Math.min(68, Math.floor(w/6)));
  const gap = 2.5, barW = (w - (count-1)*gap)/count;
  let frequencies = null;
  if (m?.analyser) {
    frequencies = m.frequencyData ||= new Uint8Array(m.analyser.frequencyBinCount);
    m.analyser.getByteFrequencyData(frequencies);
  }
  for (let i=0;i<count;i++) {
    const freq = 80 * Math.pow(100, i / Math.max(1,count-1));
    const bin = frequencies ? Math.max(1,Math.min(frequencies.length-1,Math.round(freq * m.analyser.fftSize / m.ctx.sampleRate))) : 0;
    let v = frequencies ? frequencies[bin]/255 : 0;
    v = Math.min(1,Math.pow(v,1.55)*1.26);
    const height = Math.max(2, v*(h-8));
    ctx.fillStyle = i > count*.72 ? 'rgba(158,197,182,.68)' : 'rgba(142,201,181,.84)';
    ctx.fillRect(i*(barW+gap),h-height,barW,height);
  }
}
function runMeter() {
  stopSpectrum();
  const frame = () => {
    const m = media || previewMedia;
    if (!m) { stopSpectrum(); updateMicDisplay(); return; }
    const arr = m.timeData ||= new Uint8Array(m.analyser.fftSize);
    m.analyser.getByteTimeDomainData(arr);
    let power = 0; for (let i=0;i<arr.length;i++) power += ((arr[i]-128)/128)**2;
    const rms = Math.sqrt(power/arr.length);
    ui.levelFill.style.width = `${Math.min(100,Math.max(0,rms*430))}%`;
    drawSpectrum(m);
    meterFrame = requestAnimationFrame(frame);
  };
  updateMicDisplay(); meterFrame = requestAnimationFrame(frame);
}
async function startPreviewMic() {
  if (media || previewMedia) return;
  const stream = await navigator.mediaDevices.getUserMedia({ audio: selectedAudioConstraint(), video: false });
  const ctx = new AudioContext();
  try {
    const source = ctx.createMediaStreamSource(stream);
    const analyser = ctx.createAnalyser(); analyser.fftSize = 2048; analyser.smoothingTimeConstant = .84;
    source.connect(analyser);
    // A local test never connects to the speakers or the peer, preventing feedback.
    await ctx.resume(); previewMedia = { stream, ctx, source, analyser };
    await refreshAudioDevices(); runMeter();
  } catch (e) { stream.getTracks().forEach(t=>t.stop()); await ctx.close(); throw e; }
}
async function stopPreviewMic() {
  const m = previewMedia; previewMedia = null;
  if (m) { m.stream.getTracks().forEach(t=>t.stop()); await m.ctx.close().catch(()=>{}); }
  if (media) runMeter(); else { stopSpectrum(); updateMicDisplay(); }
}

// The mascot is playful without becoming a distracting screensaver.
function wireGhosts() {
  const reduced = window.matchMedia('(prefers-reduced-motion: reduce)');
  const ghosts = () => [...document.querySelectorAll('.ghost-creature')];
  let pointer = null;
  window.addEventListener('pointermove', e => {
    if (e.pointerType === 'touch') return;
    pointer = { x:e.clientX, y:e.clientY };
  }, { passive:true });
  const blink = (g) => { g.classList.add('ghost-blink'); setTimeout(()=>g.classList.remove('ghost-blink'),190); };
  function behavior() {
    for (const g of ghosts()) {
      if (reduced.matches) { g.style.setProperty('--look-x','0px'); g.style.setProperty('--look-y','0px'); continue; }
      const box = g.getBoundingClientRect(); const x=box.left+box.width*.5, y=box.top+box.height*.46;
      const inRange = pointer && Math.hypot(pointer.x-x,pointer.y-y) < (g.id==='heroGhost'?480:270);
      const dx = inRange ? Math.max(-5,Math.min(5,(pointer.x-x)/35)) : 0;
      const dy = inRange ? Math.max(-4,Math.min(4,(pointer.y-y)/40)) : 0;
      g.style.setProperty('--look-x', `${dx.toFixed(1)}px`); g.style.setProperty('--look-y', `${dy.toFixed(1)}px`);
    }
    setTimeout(behavior, 65);
  }
  behavior();
  const moods = ['ghost-sly','ghost-peek','ghost-dart'];
  function idleBehavior() {
    if (!reduced.matches) for (const g of ghosts()) {
      if (Math.random() < .48) blink(g);
      if (Math.random() < .33) {
        const mood = moods[Math.floor(Math.random()*moods.length)];
        g.classList.add(mood); setTimeout(()=>g.classList.remove(mood),900);
      }
    }
    setTimeout(idleBehavior, 2600 + Math.random()*3500);
  }
  idleBehavior();
  document.addEventListener('click',e=>{
    const g=e.target.closest('.ghost-creature'); if (!g || reduced.matches) return;
    g.classList.remove('ghost-dart'); void g.offsetWidth; g.classList.add('ghost-dart');
    setTimeout(()=>g.classList.remove('ghost-dart'),900);
  });
}


async function api(path, opts = {}) {
  const headers = { ...(identity ? { 'X-User-ID': identity.id, 'X-Auth-Token': identity.token, 'X-Session-ID': sessionId } : {}) };
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(path, { ...opts, cache: 'no-store', headers: { ...headers, ...(opts.headers || {}) }, body: opts.body === undefined ? undefined : JSON.stringify(opts.body) });
  if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
  return res.json();
}
async function openHistoryDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open('quietlink-local-v1', 1);
    req.onupgradeneeded = () => {
      const store = req.result.createObjectStore('history', { keyPath: 'key' });
      store.createIndex('peer', 'peer', { unique: false });
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}
async function saveEntry(entry) {
  if (!localDB || !identity) return;
  entry.key = `${identity.id}:${entry.peer}:${entry.id}`;
  return new Promise((resolve, reject) => {
    const tx = localDB.transaction('history', 'readwrite');
    tx.objectStore('history').put(entry);
    tx.oncomplete = () => resolve(); tx.onerror = () => reject(tx.error);
  });
}
async function historyFor(peerId) {
  if (!localDB || !identity) return [];
  return new Promise((resolve, reject) => {
    const tx = localDB.transaction('history', 'readonly');
    const req = tx.objectStore('history').index('peer').getAll(IDBKeyRange.only(`${identity.id}:${peerId}`));
    // v1 index is on "peer"; ensure entries use a namespaced peer index.
    req.onsuccess = () => resolve(req.result.sort((a,b) => a.ts - b.ts));
    req.onerror = () => reject(req.error);
  });
}
function recordEntry(entry) {
  entry.peer = `${identity.id}:${entry.contact}`;
  return saveEntry(entry).then(() => { if (entry.contact === selectedId) renderHistory(); });
}
function currentFriend(id) { return friendState.friends.find(f => f.id === id); }
function setSelected(id, name) {
  selectedId = id; selectedName = name || currentFriend(id)?.name || id || '';
  ui.conversationTitle.textContent = selectedName || 'Your space, your signal.';
  ui.headerMode.textContent = id ? (peer?.id === id ? 'PRIVATE CHANNEL' : 'CONTACT') : 'STANDBY';
  const f = currentFriend(id);
  if (f) ui.dial.value = f.online && f.callId ? f.callId : '';
  renderFriends(); renderHistory(); updateState();
  if (window.matchMedia('(max-width: 760px)').matches) { $('app').dataset.mobileView='chat'; document.querySelectorAll('.mobile-nav button').forEach(b=>b.classList.toggle('active',b.dataset.view==='chat')); }
}
function renderFriends() {
  ui.friends.replaceChildren(); ui.friendCount.textContent = String(friendState.friends.length);
  if (!friendState.friends.length) ui.friends.append(node('div', 'empty-small', 'Your contacts will appear here.'));
  for (const f of friendState.friends) {
    const item = node('button', `contact${selectedId === f.id ? ' selected' : ''}`);
    item.type = 'button';
    item.append(node('span', 'contact-mark', f.name.charAt(0).toUpperCase()));
    const info = node('span', 'contact-info'); info.append(node('span', 'contact-name', f.name), node('span', 'contact-sub', f.online ? f.callId : 'OFFLINE'));
    item.append(info, node('span', `online-indicator${f.online ? ' live' : ''}`));
    item.addEventListener('click', () => setSelected(f.id, f.name)); ui.friends.append(item);
  }
}
function renderInbox() {
  ui.inbox.replaceChildren(); ui.inboxCount.textContent = String(friendState.incoming.length);
  if (!friendState.incoming.length && !friendState.outgoing.length) ui.inbox.append(node('div', 'empty-small', 'No new friend requests.'));
  for (const f of friendState.incoming) {
    const item = node('div', 'request'); item.append(node('strong', '', f.name), node('small', '', f.id));
    const actions = node('div', 'request-actions');
    for (const [title, accept] of [['Accept', true], ['Decline', false]]) {
      const b = node('button', '', title); b.type = 'button';
      b.addEventListener('click', async () => {
        b.disabled = true;
        try { await api('/api/friend/respond', { method: 'POST', body: { id: f.id, accept } }); await refreshFriends(); toast(accept ? 'Added to contacts' : 'Request declined'); }
        catch (e) { toast(e.message, true); b.disabled = false; }
      }); actions.append(b);
    }
    item.append(actions); ui.inbox.append(item);
  }
  if (friendState.outgoing.length) {
    const label = node('div', 'pending-label', 'SENT INVITES'); ui.inbox.append(label);
    for (const f of friendState.outgoing) { const row = node('div', 'pending-request'); row.append(node('span', '', f.name), node('small', '', 'Awaiting response')); ui.inbox.append(row); }
  }
}
async function refreshFriends() {
  if (!identity) return;
  try { friendState = await api('/api/friends'); renderFriends(); renderInbox(); }
  catch (e) { console.warn('Friends refresh:', e); }
}
function updateState() {
  const open = control?.readyState === 'open';
  const connecting = !!pc || !!outgoingInvite;
  const direct = !!pc && (pc.connectionState === 'connected' || open);
  ui.connect.disabled = !hubOnline || connecting;
  ui.voice.disabled = !open;
  ui.hangup.disabled = !connecting;
  ui.send.disabled = !open; ui.message.disabled = !open; ui.attach.disabled = !open;
  ui.voice.textContent = media ? 'Mute mic' : 'Start voice';
  ui.message.placeholder = open ? 'Write a message…' : 'Connect to a peer to send a message...';
  ui.connectionLabel.textContent = open ? 'P2P connected' : connecting ? 'Connecting' : 'Not connected';
  ui.connectionDot.classList.toggle('live', open || direct);
  ui.iceState.textContent = pc?.iceConnectionState || '—';
  ui.dataState.textContent = control?.readyState || 'Closed';
  ui.voiceState.textContent = media ? 'Transmitting' : 'Muted';
  updateMicDisplay();
  ui.hubState.textContent = hubOnline ? 'Online' : 'Offline';
  ui.selfStatus.textContent = hubOnline ? 'Available for connections' : 'Not connected to hub';
  ui.presenceDot.classList.toggle('live', hubOnline);
  if (peer && selectedId === peer.id) ui.headerMode.textContent = open ? 'PRIVATE CHANNEL' : 'CONNECTING';
}
async function initAccount() {
  try { identity = JSON.parse(localStorage.getItem('quietlink.identity') || 'null'); } catch { identity = null; }
  if (identity?.id && identity?.token) {
    try { const me = await api('/api/me'); identity.name = me.name; localStorage.setItem('quietlink.identity', JSON.stringify(identity)); }
    catch { identity = null; toast('Identity unavailable on this hub; restore or create one', true); }
  } else identity = null;
  if (!identity) { $('onboarding').hidden = false; return; }
  $('onboarding').hidden = true;
  ui.selfName.textContent = identity.name;
  ui.selfAvatar.textContent = identity.name.charAt(0).toUpperCase();
  ui.mailId.textContent = identity.id;
  await refreshFriends();
  startSSE();
}
async function startSSE() {
  if (streamAbort) streamAbort.abort();
  const controller = new AbortController(); streamAbort = controller;
  const headers = { 'X-User-ID': identity.id, 'X-Auth-Token': identity.token, 'X-Session-ID': sessionId };
  try {
    const response = await fetch('/api/events', { headers, signal: controller.signal, cache: 'no-store' });
    if (!response.ok) throw new Error(`Presence unavailable (${response.status})`);
    const reader = response.body.getReader(); const decoder = new TextDecoder(); let buf = '';
    while (true) {
      const { value, done } = await reader.read(); if (done) break;
      buf += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n');
      if (buf.length > 500000) throw new Error('Presence stream overflow');
      let idx;
      while ((idx = buf.indexOf('\n\n')) !== -1) {
        const frame = buf.slice(0, idx); buf = buf.slice(idx + 2);
        const line = frame.split('\n').find(l => l.startsWith('data: '));
        if (line) { try { await onHubEvent(JSON.parse(line.slice(6))); } catch (e) { console.warn('Hub event:', e); } }
      }
    }
  } catch (e) { if (!controller.signal.aborted) console.warn('Presence disconnected:', e); }
  if (controller.signal.aborted) return;
  hubOnline = false; callId = ''; ui.callId.textContent = 'OFFLINE'; updateState();
  clearTimeout(sseRestart); sseRestart = setTimeout(() => { if (identity) startSSE(); }, 2000);
}
async function onHubEvent(event) {
  if (event.type === 'hello') {
    callId = event.data.callId; hubOnline = true;
    ui.callId.textContent = callId; friendState = event.data.friends || friendState;
    renderFriends(); renderInbox(); updateState();
  } else if (event.type === 'presence' || event.type === 'inbox') {
    await refreshFriends();
  } else if (event.type === 'signal') {
    await onSignal(event.data);
  }
}
async function sendSignal(to, kind, data = null) {
  return api('/api/signal', { method: 'POST', body: { to, kind, data } });
}
function showIncoming(msg) {
  currentInvite = msg;
  ui.incomingName.textContent = `${msg.fromName} is calling`;
  ui.incomingText.textContent = `${msg.fromCallId} · direct voice / messages / files`;
  ui.incoming.classList.remove('hidden');
  if ('Notification' in window && Notification.permission === 'granted' && document.visibilityState !== 'visible') {
    new Notification('Ghost', { body: `Incoming connection from ${msg.fromName}` });
  }
}
function hideIncoming() { currentInvite = null; ui.incoming.classList.add('hidden'); }
async function onSignal(msg) {
  if (!msg || !msg.fromCallId || !msg.fromUserId) return;
  if (msg.kind === 'invite') {
    if (pc || outgoingInvite || currentInvite) { await sendSignal(msg.fromCallId, 'decline', { reason: 'busy' }).catch(() => {}); return; }
    showIncoming(msg); return;
  }
  if (msg.kind === 'accept') {
    if (!outgoingInvite || outgoingInvite !== msg.fromCallId || pc) return;
    outgoingInvite = null; clearTimeout(inviteTimeout);
    try { await startPeer(msg, true); }
    catch (e) { toast(`Connection error: ${e.message}`, true); disconnect(false); }
    return;
  }
  if (msg.kind === 'decline') {
    if (outgoingInvite === msg.fromCallId) { outgoingInvite = null; clearTimeout(inviteTimeout); toast('Connection declined or peer is busy'); updateState(); }
    return;
  }
  if (msg.kind === 'hangup') {
    if (peer?.callId === msg.fromCallId) { disconnect(false); toast('Peer disconnected'); }
    return;
  }
  if (!pc || !peer || msg.fromCallId !== peer.callId) return;
  try {
    if (msg.kind === 'offer') {
      await pc.setRemoteDescription(msg.data);
      // The answerer must negotiate a send-capable audio transceiver as well.
      // Without this, replacing the track later only transmits on the caller's side.
      const audio = pc.getTransceivers().find(t => t.receiver.track.kind === 'audio');
      if (audio) audio.direction = 'sendrecv';
      await flushICE();
      const answer = await pc.createAnswer(); await pc.setLocalDescription(answer);
      await sendSignal(peer.callId, 'answer', pc.localDescription);
    } else if (msg.kind === 'answer') {
      await pc.setRemoteDescription(msg.data); await flushICE();
    } else if (msg.kind === 'ice' && msg.data?.candidate) {
      if (pc.remoteDescription) await pc.addIceCandidate(msg.data);
      else queuedICE.push(msg.data);
    }
  } catch (e) { toast(`WebRTC signaling: ${e.message}`, true); console.error(e); }
}
async function flushICE() {
  for (const candidate of queuedICE.splice(0)) {
    try { await pc.addIceCandidate(candidate); } catch (e) { console.warn('ICE candidate:', e); }
  }
}
async function startPeer(msg, initiator) {
  if (pc) throw Error('Connection already active');
  peer = { callId: msg.fromCallId, id: msg.fromUserId, name: msg.fromName };
  activeCallSerial++; const mySerial = activeCallSerial;
  queuedICE = [];
  pc = new RTCPeerConnection({ iceServers });
  const connection = pc;
  setSelected(peer.id, peer.name);
  connection.onicecandidate = ({ candidate }) => { if (candidate && peer && mySerial === activeCallSerial) sendSignal(peer.callId, 'ice', candidate.toJSON()).catch(console.warn); };
  connection.oniceconnectionstatechange = updateState;
  connection.onconnectionstatechange = () => {
    updateState();
    if (connection.connectionState === 'failed' && connection === pc) { toast('Peer link failed. A TURN relay may be required.', true); disconnect(true); }
  };
  connection.ontrack = async ({ streams, track }) => {
    if (track.kind !== 'audio') return;
    ui.remoteAudio.srcObject = streams[0] || new MediaStream([track]);
    try { await ui.remoteAudio.play(); } catch { toast('Click Start voice / the page to allow incoming audio'); }
  };
  connection.ondatachannel = ({ channel }) => {
    if (channel.label === 'control') bindControl(channel);
    else if (channel.label.startsWith('file:')) bindIncomingFile(channel);
    else channel.close();
  };
  if (initiator) {
    connection.addTransceiver('audio', { direction: 'sendrecv' });
    bindControl(connection.createDataChannel('control', { ordered: true }));
    const offer = await connection.createOffer(); await connection.setLocalDescription(offer);
    await sendSignal(peer.callId, 'offer', connection.localDescription);
  }
  updateState();
}
function bindControl(channel) {
  control = channel;
  channel.onopen = () => { toast(`Connected directly to ${peer?.name || 'peer'}`); updateState(); renderHistory(); };
  channel.onclose = () => { updateState(); if (pc) toast('Data channel closed'); };
  channel.onerror = () => updateState();
  channel.onmessage = async ({ data }) => {
    if (typeof data !== 'string' || data.length > 65536) return;
    try { await onControl(JSON.parse(data)); } catch (e) { console.error('P2P message:', e); toast('Could not process peer message', true); }
  };
  updateState();
}
function sendControl(payload) {
  if (!control || control.readyState !== 'open') throw Error('Peer channel is not open');
  control.send(JSON.stringify(payload));
}
async function onControl(msg) {
  if (!peer || !msg || typeof msg.type !== 'string') return;
  if (msg.type === 'message') {
    if (typeof msg.body !== 'string' || !msg.body.trim() || msg.body.length > 12000 || typeof msg.id !== 'string') return;
    await recordEntry({ id: msg.id.slice(0, 64), contact: peer.id, direction: 'in', type: 'text', body: msg.body, ts: Date.now() });
  } else if (msg.type === 'file-offer') {
    if (typeof msg.id !== 'string' || !/^[a-f0-9-]{36}$/.test(msg.id) || typeof msg.name !== 'string' || msg.name.length > 250 || !Number.isSafeInteger(msg.size) || msg.size < 0) return;
    availableFiles.set(msg.id, { ...msg, peerId: peer.id });
    await recordEntry({ id: msg.id, contact: peer.id, direction: 'in', type: 'file', name: msg.name, size: msg.size, mime: msg.mime, status: 'offered', ts: Date.now() });
  } else if (msg.type === 'file-request') {
    const offer = sentFiles.get(msg.id);
    if (!offer || offer.peerId !== peer.id || offer.sending) return;
    offer.sending = true;
    sendFile(msg.id, offer).catch(async e => {
      toast(`File transfer failed: ${e.message}`, true);
      offer.sending = false;
      await updateFileStatus(msg.id, offer.peerId, 'interrupted');
    });
  } else if (msg.type === 'file-received') {
    const offer = sentFiles.get(msg.id);
    if (offer?.peerId === peer.id) {
      clearTimeout(offer.cleanupTimer);
      if (offer.channel?.readyState === 'open') offer.channel.close();
      await updateFileStatus(msg.id, peer.id, 'received by peer'); sentFiles.delete(msg.id);
    }
  } else if (msg.type === 'file-cancel') {
    const offer = sentFiles.get(msg.id);
    if (offer?.channel) offer.channel.close();
  }
}
async function updateFileStatus(id, peerId, status) {
  const entries = await historyFor(peerId);
  const entry = entries.find(e => e.id === id);
  if (!entry) return;
  entry.status = status; entry.contact = peerId;
  await recordEntry(entry);
}
async function renderHistory() {
  const renderID = ++renderVersion;
  if (!selectedId) { renderEmpty(true); return; }
  let items;
  try { items = await historyFor(selectedId); } catch (e) { toast('Local history unavailable', true); return; }
  if (renderID !== renderVersion) return;
  if (!items.length) { renderEmpty(false); return; }
  const frag = document.createDocumentFragment();
  for (const msg of items.slice(-500)) {
    const mine = msg.direction === 'out'; const wrapper = node('div', `message ${mine ? 'mine' : 'theirs'}`);
    if (!mine) wrapper.append(node('span', 'contact-mark', selectedName.charAt(0).toUpperCase() || '?'));
    const bubble = node('div', `bubble ${msg.type === 'file' ? 'file-bubble' : ''}`);
    if (msg.type === 'text') {
      bubble.append(node('div', 'bubble-text', msg.body));
    } else if (msg.type === 'file') {
      bubble.append(node('div', 'file-flag', mine ? 'FILE OFFERED' : 'INCOMING FILE'));
      const info = node('div', 'file-info'); const texts = node('div');
      texts.append(node('div', 'file-name', msg.name), node('div', 'file-size', bytes(msg.size)));
      info.append(node('div', 'file-icon', 'FILE'), texts); bubble.append(info);
      const actions = node('div', 'file-actions');
      let status = msg.status || 'offered';
      if (mine && !sentFiles.has(msg.id) && ['offered','interrupted'].includes(status)) status = 'Expired • reoffer';
      if (!mine && status === 'offered' && !availableFiles.has(msg.id)) status = 'Expired • ask to reoffer';
      actions.append(node('span', 'file-status', status));
      if (!mine && status === 'offered' && availableFiles.has(msg.id)) {
        const download = node('button', 'file-download', 'Download'); download.type = 'button';
        download.disabled = !(peer?.id === selectedId && control?.readyState === 'open');
        download.addEventListener('click', () => requestFile(msg, download)); actions.append(download);
      }
      bubble.append(actions);
    }
    bubble.append(node('div', 'bubble-meta', formatTime(msg.ts)));
    wrapper.append(bubble); frag.append(wrapper);
  }
  ui.history.replaceChildren(frag); ui.history.scrollTop = ui.history.scrollHeight;
}
function renderEmpty(noContact) {
  const empty = node('div', 'empty-state');
  const symbol = node('div', 'wave-symbol'); for (let i = 0; i < 5; i++) symbol.append(node('span'));
  const hero = $('brandGhost').cloneNode(true); hero.removeAttribute('id'); hero.id='heroGhost'; hero.classList.remove('brand-creature'); hero.classList.add('hero-creature');
  empty.append(hero, node('h1', '', noContact ? 'Present. Until you need me.' : 'Just the two of you.'),
    node('p', '', noContact ? 'Pick a person, or dial their live ID. The rest stays between you.' : 'When you connect, your messages and transfers live here.'));
  const list = node('div', 'empty-metadata');
  for (const [number, label] of [['01','DIRECT VOICE'],['02','MESSAGES'],['03','ON-DEMAND FILES']]) {
    const item = node('span'); item.append(node('b', '', number), document.createTextNode(label)); list.append(item);
  }
  empty.append(list); ui.history.replaceChildren(empty);
}
async function sendMessage(event) {
  event.preventDefault(); if (!peer || control?.readyState !== 'open') return;
  const body = ui.message.value.trim(); if (!body) return;
  const id = makeID();
  try {
    sendControl({ type: 'message', id, body });
    await recordEntry({ id, contact: peer.id, direction: 'out', type: 'text', body, ts: Date.now() });
    ui.message.value = '';
  } catch (e) { toast(e.message, true); }
}
async function offerFile(file) {
  if (!file || !peer || control?.readyState !== 'open') return;
  const id = makeID(); const peerId = peer.id;
  sentFiles.set(id, { file, peerId, sending: false });
  try {
    sendControl({ type: 'file-offer', id, name: file.name, size: file.size, mime: file.type });
    await recordEntry({ id, contact: peerId, direction: 'out', type: 'file', name: file.name, size: file.size, mime: file.type, status: 'offered', ts: Date.now() });
    toast('Offer sent. File bytes remain local until Download is requested.');
  } catch (e) { sentFiles.delete(id); toast(e.message, true); }
}
async function requestFile(msg, button) {
  if (!peer || msg.contact !== peer.id || control?.readyState !== 'open' || receivingFiles.has(msg.id)) return;
  const offer = availableFiles.get(msg.id); if (!offer) return;
  let writable = null, nativeDownload = null;
  if (window.GhostAndroid?.beginDownload) {
    nativeDownload = window.GhostAndroid.beginDownload(msg.name, offer.mime || 'application/octet-stream');
    if (!nativeDownload) { toast('Android could not create the download file', true); return; }
  } else if (window.showSaveFilePicker) {
    try {
      const handle = await window.showSaveFilePicker({ suggestedName: msg.name.replace(/[\\/]/g, '_') });
      writable = await handle.createWritable();
    } catch (e) { if (e.name !== 'AbortError') toast(`Cannot select save location: ${e.message}`, true); return; }
  } else if (msg.size > 100 * 1024 * 1024) {
    toast('This browser needs a download of 100 MB or less; use Chrome / Edge for large streamed files.', true); return;
  }
  button.disabled = true;
  receivingFiles.set(msg.id, { id: msg.id, peerId: peer.id, expected: msg.size, received: 0, chunks: [], writable, nativeDownload, name: msg.name, finished: false });
  try { await updateFileStatus(msg.id, peer.id, 'requesting'); sendControl({ type: 'file-request', id: msg.id }); }
  catch (e) { receivingFiles.delete(msg.id); try { await writable?.abort(); } catch {} if(nativeDownload) window.GhostAndroid.abortDownload(nativeDownload); toast(e.message, true); }
}
function bindIncomingFile(channel) {
  channel.binaryType = 'arraybuffer';
  const id = channel.label.slice(5);
  const transfer = receivingFiles.get(id);
  if (!transfer || !peer || transfer.peerId !== peer.id) { channel.close(); return; }
  let chain = Promise.resolve();
  channel.onmessage = ({ data }) => {
    chain = chain.then(async () => {
      if (transfer.finished) return;
      if (typeof data === 'string') {
        if (data !== '__QUIETLINK_END__' || transfer.received !== transfer.expected) throw Error('Incomplete file');
        transfer.finished = true;
        if (transfer.writable) await transfer.writable.close();
        else if (transfer.nativeDownload) {
          if (!window.GhostAndroid.completeDownload(transfer.nativeDownload)) throw Error('Could not finalize Android download');
        } else {
          const url = URL.createObjectURL(new Blob(transfer.chunks));
          const a = document.createElement('a'); a.href = url; a.download = transfer.name; document.body.append(a); a.click(); a.remove();
          setTimeout(() => URL.revokeObjectURL(url), 60000);
        }
        receivingFiles.delete(id); availableFiles.delete(id);
        await updateFileStatus(id, transfer.peerId, 'downloaded');
        try { sendControl({ type: 'file-received', id }); } catch {}
        toast(`Saved ${transfer.name}`);
      } else {
        const buf = data instanceof ArrayBuffer ? data : await data.arrayBuffer();
        transfer.received += buf.byteLength;
        if (transfer.received > transfer.expected) throw Error('Received more bytes than advertised');
        if (transfer.writable) await transfer.writable.write(buf);
        else if (transfer.nativeDownload) {
          const bytes = new Uint8Array(buf); let encoded = '';
          for (let off=0;off<bytes.length;off+=8192) encoded += String.fromCharCode(...bytes.subarray(off,off+8192));
          if (!window.GhostAndroid.appendDownload(transfer.nativeDownload, btoa(encoded))) throw Error('Android download write failed');
        } else transfer.chunks.push(buf);
        if (transfer.received % (1024 * 1024) < buf.byteLength || transfer.received === transfer.expected) {
          await updateFileStatus(id, transfer.peerId, `${Math.round(100 * transfer.received / (transfer.expected || 1))}%`);
        }
      }
    }).catch(async (error) => {
      if (transfer.finished) return;
      transfer.finished = true;
      try { await transfer.writable?.abort(); } catch {}
      if (transfer.nativeDownload) window.GhostAndroid.abortDownload(transfer.nativeDownload);
      receivingFiles.delete(id); channel.close();
      await updateFileStatus(id, transfer.peerId, 'interrupted'); toast(`Download failed: ${error.message}`, true);
    });
  };
  channel.onclose = () => {
    chain.then(async () => {
      if (!transfer.finished) {
        transfer.finished = true; try { await transfer.writable?.abort(); } catch {}
        if (transfer.nativeDownload) window.GhostAndroid.abortDownload(transfer.nativeDownload);
        receivingFiles.delete(id); await updateFileStatus(id, transfer.peerId, 'interrupted');
      }
    });
  };
}
async function sendFile(id, offer) {
  if (!pc || !peer || offer.peerId !== peer.id) throw Error('Peer went away');
  const channel = pc.createDataChannel(`file:${id}`, { ordered: true }); offer.channel = channel;
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(Error('File channel timed out')), 15000);
    channel.onopen = () => { clearTimeout(timer); resolve(); };
    channel.onerror = () => { clearTimeout(timer); reject(Error('File channel error')); };
    channel.onclose = () => { clearTimeout(timer); reject(Error('Peer closed file channel')); };
  });
  const { file } = offer;
  const chunkSize = Math.min(16384, pc.sctp?.maxMessageSize || 16384);
  for (let pos = 0; pos < file.size; pos += chunkSize) {
    if (channel.readyState !== 'open') throw Error('Connection lost during transfer');
    while (channel.bufferedAmount > 262144) {
      await new Promise((resolve) => { const t = setTimeout(resolve, 180); channel.bufferedAmountLowThreshold = 131072; channel.addEventListener('bufferedamountlow', () => { clearTimeout(t); resolve(); }, { once: true }); });
      if (channel.readyState !== 'open') throw Error('Connection lost during transfer');
    }
    channel.send(await file.slice(pos, pos + chunkSize).arrayBuffer());
  }
  await updateFileStatus(id, offer.peerId, 'transmitted • awaiting receipt');
  channel.send('__QUIETLINK_END__');
  while (channel.readyState === 'open' && channel.bufferedAmount > 0) await sleep(100);
  // Do not close before the recipient acknowledges the saved file. A short
  // arbitrary delay can truncate slow transfers on constrained connections.
  offer.cleanupTimer = setTimeout(() => { if (channel.readyState === 'open') channel.close(); }, 120000);
  offer.sending = false;
}
async function enableMic() {
  if (!pc) return;
  if (previewMedia) await stopPreviewMic();
  const stream = await navigator.mediaDevices.getUserMedia({ audio: selectedAudioConstraint(), video: false });
  const ctx = new AudioContext();
  try {
    const source = ctx.createMediaStreamSource(stream);
    const gain = ctx.createGain(); gain.gain.value = gainFactor();
    const analyser = ctx.createAnalyser(); analyser.fftSize = 2048; analyser.smoothingTimeConstant = .84; source.connect(analyser);
    const dst = ctx.createMediaStreamDestination();
    source.connect(gain);
    let gate = null;
    try { await ctx.audioWorklet.addModule('/static/gate-processor.js'); gate = new AudioWorkletNode(ctx, 'snr-gate'); gate.port.postMessage({ thresholdDb: Number(ui.snr.value) }); gain.connect(gate); gate.connect(dst); }
    catch (e) { gain.connect(dst); toast('Noise gate not available in this browser; gain still works', true); console.warn(e); }
    const track = dst.stream.getAudioTracks()[0];
    const sender = pc.getTransceivers().find(t => t.receiver.track.kind === 'audio')?.sender;
    if (!sender) throw Error('No negotiated audio sender');
    await sender.replaceTrack(track);
    await ctx.resume();
    media = { stream, ctx, source, gain, gate, analyser, dst, track, sender };
    await refreshAudioDevices();
    runMeter(); updateState();
  } catch (e) {
    stream.getTracks().forEach(t => t.stop()); await ctx.close(); throw e;
  }
}
async function disableMic() {
  const m = media; media = null;
  if (!m) return;
  try { await m.sender.replaceTrack(null); } catch {}
  m.stream.getTracks().forEach(t => t.stop()); m.track.stop();
  await m.ctx.close().catch(() => {});
  if (previewMedia) runMeter(); else { stopSpectrum(); updateMicDisplay(); } updateState();
}
async function disconnect(notify = true) {
  if (notify && (peer || outgoingInvite)) await sendSignal(peer?.callId || outgoingInvite, 'hangup').catch(() => {});
  hideIncoming(); outgoingInvite = null; clearTimeout(inviteTimeout); activeCallSerial++;
  await disableMic();
  const old = pc; pc = null; peer = null; queuedICE = [];
  if (control) { control.onclose = null; control.close(); } control = null;
  if (old) old.close();
  for (const r of receivingFiles.values()) { try { await r.writable?.abort(); } catch {} if(r.nativeDownload) window.GhostAndroid.abortDownload(r.nativeDownload); }
  receivingFiles.clear(); sentFiles.clear(); availableFiles.clear();
  ui.remoteAudio.srcObject = null; updateState(); renderHistory();
}
function backupIdentity() {
  if (!identity) return;
  const content = JSON.stringify({ app: 'Ghost', id: identity.id, name: identity.name, token: identity.token }, null, 2);
  const url = URL.createObjectURL(new Blob([content], { type: 'application/json' }));
  const anchor = document.createElement('a'); anchor.href = url; anchor.download = `quietlink-key-${identity.id}.json`;
  anchor.click(); setTimeout(() => URL.revokeObjectURL(url), 5000);
  toast('Keep this access key private. Anyone holding it can use your identity.');
}
// The peer directory lives on the local Go node, not on a hosted Ghost hub.
let meshRefreshRunning = false;
async function refreshMesh() {
  if (!identity || meshRefreshRunning) return;
  meshRefreshRunning = true;
  try {
    const state = await api('/api/peers');
    const items = state.peers || [];
    $('meshCount').textContent = String(items.filter(p=>p.online).length);
    $('meshSelf').textContent = (state.addresses || []).find(a => !a.includes('127.0.0.1')) || state.address || 'Unknown address';
    const target = $('meshPeers'); target.replaceChildren();
    if (!items.length) { target.append(node('div', 'mesh-empty', 'Searching LAN · or enter a peer address')); return; }
    for (const p of items) {
      const group = node('div', 'mesh-peer');
      const title = node('div', 'mesh-peer-top');
      const name = node('strong', '', p.url.replace(/^https?:\/\//, ''));
      const status = node('span', 'node-state', p.online ? 'ONLINE' : 'OFFLINE');
      title.append(name, status); group.append(title);
      if (p.online && p.users?.length) {
        const users = node('div', 'mesh-peer-users');
        for (const user of p.users) {
          const button = node('button', '', `+ ${user.name}`); button.type = 'button';
          button.title = `${user.id} · invite to contacts`;
          button.disabled = user.id === identity.id || (friendState.friends || []).some(f=>f.id === user.id);
          button.addEventListener('click', async () => {
            button.disabled = true;
            try { await api('/api/friend/request', { method: 'POST', body: { id: user.id } }); await refreshFriends(); toast(`Invitation sent to ${user.name}`); }
            catch (err) { toast(err.message, true); button.disabled = false; }
          });
          users.append(button);
        }
        group.append(users);
      }
      target.append(group);
    }
  } catch (e) { $('meshPeers').replaceChildren(node('div','mesh-empty', 'Peer discovery unavailable')); }
  finally { meshRefreshRunning = false; }
}
async function wirePeerDiscovery() {
  $('copyMeshSelf').addEventListener('click', () => copy($('meshSelf').textContent));
  $('meshConnectForm').addEventListener('submit', async e => {
    e.preventDefault(); const input = $('meshAddress'); const address = input.value.trim(); if (!address) return;
    try { await api('/api/peers/connect',{ method:'POST',body:{address} }); input.value=''; await refreshMesh(); await refreshFriends(); toast('Peer found. You can add their Mail ID.'); }
    catch (err) { toast(`Cannot reach peer: ${err.message}`, true); }
  });
  await refreshMesh();
  setInterval(refreshMesh, 6500);
}

async function wireEvents() {
  $('copyMail').addEventListener('click', () => copy(identity?.id));
  $('copyCall').addEventListener('click', () => copy(callId));
  $('backupIdentity').addEventListener('click', backupIdentity);
  $('addFriendForm').addEventListener('submit', async (e) => {
    e.preventDefault(); const input = $('friendIdInput'); const id = input.value.trim().toUpperCase();
    try { await api('/api/friend/request', { method: 'POST', body: { id } }); input.value = ''; await refreshFriends(); toast('Invitation sent to the inbox'); }
    catch (err) { toast(err.message, true); }
  });
  ui.connect.addEventListener('click', async () => {
    const to = ui.dial.value.trim().toUpperCase();
    if (!/^[A-Z2-7]{9}$/.test(to)) { toast('Enter a valid 9-character live Call ID', true); return; }
    if (to === callId) { toast('You cannot connect to your own Call ID', true); return; }
    if (pc || outgoingInvite) return;
    outgoingInvite = to; updateState();
    try { await sendSignal(to, 'invite'); toast('Connection request sent'); inviteTimeout = setTimeout(() => { if (outgoingInvite === to) { outgoingInvite = null; updateState(); toast('Connection request timed out'); } }, 35000); }
    catch (e) { outgoingInvite = null; updateState(); toast(e.message, true); }
  });
  $('acceptCall').addEventListener('click', async () => {
    const invite = currentInvite; if (!invite) return; hideIncoming();
    try { await startPeer(invite, false); await sendSignal(invite.fromCallId, 'accept'); }
    catch (e) { await disconnect(false); toast(e.message, true); }
  });
  $('declineCall').addEventListener('click', async () => {
    const invite = currentInvite; hideIncoming();
    if (invite) await sendSignal(invite.fromCallId, 'decline').catch(() => {});
  });
  ui.hangup.addEventListener('click', () => disconnect(true));
  ui.voice.addEventListener('click', async () => {
    ui.voice.disabled = true;
    try { if (media) await disableMic(); else await enableMic(); }
    catch (e) { toast(`Microphone unavailable: ${e.message}`, true); }
    finally { updateState(); }
  });
  $('composer').addEventListener('submit', sendMessage);
  ui.attach.addEventListener('click', () => ui.fileInput.click());
  ui.fileInput.addEventListener('change', async () => { const file = ui.fileInput.files?.[0]; ui.fileInput.value = ''; await offerFile(file); });
  $('onboardingForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    const name = $('nameInput').value.trim(); if (!name) return;
    try { identity = await api('/api/register', { method: 'POST', body: { name } }); localStorage.setItem('quietlink.identity', JSON.stringify(identity)); await initAccount(); }
    catch (err) { identity = null; toast(err.message, true); }
  });
  $('restoreButton').addEventListener('click', () => $('restoreInput').click());
  $('restoreInput').addEventListener('change', async (e) => {
    const file = e.target.files?.[0]; if (!file) return;
    try {
      const data = JSON.parse(await file.text());
      if (!/^[A-Z2-7]{10}$/.test(data.id) || !/^[a-f0-9]{64}$/.test(data.token)) throw Error('Not a valid Ghost access key');
      const previous = identity; identity = data;
      try { const me = await api('/api/me'); identity.name = me.name; localStorage.setItem('quietlink.identity', JSON.stringify(identity)); await initAccount(); toast('Identity restored'); }
      catch (err) { identity = previous; throw err; }
    } catch (err) { toast(err.message, true); }
    $('restoreInput').value = '';
  });
  window.addEventListener('pagehide', () => { if (streamAbort) streamAbort.abort(); });
}
async function copy(value) {
  if (!value) { toast('Not connected yet', true); return; }
  try { await navigator.clipboard.writeText(value); toast('Copied to clipboard'); }
  catch { toast('Clipboard permission not available', true); }
}
function wireMobileNavigation() {
  const buttons = [...document.querySelectorAll('.mobile-nav button')];
  const choose = (view) => {
    $('app').dataset.mobileView = view;
    buttons.forEach(b=>{ b.classList.toggle('active',b.dataset.view===view); b.setAttribute('aria-current',b.dataset.view===view?'page':'false'); });
  };
  buttons.forEach(b=>b.addEventListener('click',()=>choose(b.dataset.view)));
  choose('chat');
}
async function main() {
  initControls(); wireAudioDevices(); wireGhosts(); wireMobileNavigation(); renderEmpty(true); await wireEvents();
  try { localDB = await openHistoryDB(); }
  catch (e) { toast(`History storage unavailable: ${e.message}`, true); }
  try { const cfg = await api('/api/config'); iceServers = cfg.iceServers || []; }
  catch (e) { toast(`Hub is unavailable: ${e.message}`, true); }
  await initAccount(); await wirePeerDiscovery(); updateState();
}
main().catch(err => { console.error(err); toast(err.message, true); });
