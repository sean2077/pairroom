'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');
const source = fs.readFileSync(path.join(__dirname, '../internal/enterpriseservice/web/device.js'), 'utf8');
const flush = () => new Promise((resolve) => setImmediate(resolve));
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

class Element {
  constructor() { this.listeners = new Map(); this.textContent = ''; this.hidden = false; this.disabled = false; }
  addEventListener(type, listener) { this.listeners.set(type, listener); }
  fire(type) { return this.listeners.get(type)(); }
  set innerHTML(_value) { throw new Error('device approval must render untrusted metadata as text'); }
}

function fixture() {
  const elements = new Map();
  const element = (id) => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const requests = [], navigations = [];
  let nextID = 0;
  let currentSession = session(), channel;
  const responseHeaders = (value = currentSession) => ({ get: (name) => ({ 'X-PairRoom-User': value.user_id, 'X-PairRoom-Session': value.session_id })[name] || null });
  vm.runInNewContext(source, {
    document: { getElementById: element },
    window: { location: { pathname: '/enterprise/device-enrollments/request-1', assign: (value) => navigations.push(value) } },
    URL, AbortController, Date,
    BroadcastChannel: class { constructor() { channel = this; } },
    crypto: { randomUUID: () => `operation-${++nextID}` },
    // Ignoring AbortSignal deliberately exercises epoch guards against late
    // transport/JSON results, rather than relying on abort alone for isolation.
    fetch: (url, options) => { const result = deferred(); requests.push({ url, options, ...result }); return result.promise; }
  });
  const reply = (index, status, payload) => { if (requests[index].url.endsWith('/session') && payload.user_id) currentSession = payload; requests[index].resolve({ status, ok: status >= 200 && status < 300, headers: responseHeaders(), json: async () => payload }); };
  return { element, requests, navigations, reply, responseHeaders, broadcast: (data) => channel.onmessage({ data }), text: () => [...elements.values()].map((item) => item.textContent).join('') };
}

const session = (user = 'user-a', csrf = 'private-csrf-a') => ({ user_id: user, csrf, session_id: 'session-' + csrf, deployment_id: 'deployment-1' });
const enrollment = (label = 'My laptop', status = 'pending') => ({ request_id: 'request-1', device_id: 'device-1', label, origin: 'https://service.example.test', status, expires_at: '2030-01-01T00:00:00Z' });
const code = Buffer.alloc(32, 1).toString('base64url');
const state = Buffer.alloc(32, 2).toString('base64url');
const completion = (port = 49152) => `http://127.0.0.1:${port}/pairroom/callback?completion_code=${code}&state=${state}`;
async function ready() { const f = fixture(); f.reply(0, 200, session()); await flush(); f.reply(1, 200, enrollment()); await flush(); return f; }

test('sign-in and metadata loading never automatically approve an enrollment', async () => {
  const f = await ready();
  assert.equal(f.requests.length, 2);
  assert.equal(f.element('device-approve').disabled, false);
  assert.equal(f.navigations.length, 0);
  assert.equal(f.text().includes('private-csrf-a'), false);
  const unsigned = fixture(); unsigned.reply(0, 401, {}); await flush();
  assert.equal(unsigned.element('device-sign-in').hidden, false);
  await unsigned.element('device-approve').fire('click');
  assert.equal(unsigned.requests.length, 1);
});

test('explicit approval sends current CSRF once and navigates only to confirmed loopback completion', async () => {
  const f = await ready();
  const approval = f.element('device-approve').fire('click');
  assert.equal(f.requests[2].options.method, 'POST');
  assert.equal(f.requests[2].options.redirect, 'error');
  assert.equal(f.requests[2].options.headers['X-PairRoom-CSRF'], 'private-csrf-a');
  assert.equal(JSON.parse(f.requests[2].options.body).action, 'approve');
  assert.equal(f.navigations.length, 0);
  f.reply(2, 200, { request_id: 'request-1', completion_location: completion() });
  await approval;
  assert.deepEqual(f.navigations, [completion()]);
  assert.equal(f.text().includes(code), false);
  assert.equal(f.text().includes(state), false);
});

test('old enrollment JSON cannot reappear after account refresh', async () => {
  const f = fixture(); f.reply(0, 200, session()); await flush();
  const staleJSON = deferred(); f.requests[1].resolve({ status: 200, ok: true, headers: f.responseHeaders(), json: () => staleJSON.promise }); await flush();
  const refresh = f.element('device-refresh').fire('click');
  assert.equal(f.requests[1].options.signal.aborted, true);
  f.reply(2, 200, session('user-b', 'private-csrf-b')); await flush();
  f.reply(3, 200, enrollment('New account device')); await refresh;
  staleJSON.resolve(enrollment('Old account private label')); await flush();
  assert.equal(f.element('device-label').textContent, 'New account device');
  assert.equal(f.text().includes('Old account private label'), false);
});

test('old approval response cannot navigate after the browser changes account', async () => {
  const f = await ready();
  const approval = f.element('device-approve').fire('click');
  const refresh = f.element('device-refresh').fire('click');
  assert.equal(f.requests[2].options.signal.aborted, true);
  f.reply(3, 200, session('user-b', 'private-csrf-b')); await flush();
  f.reply(4, 403, {}); await refresh;
  f.reply(2, 200, { request_id: 'request-1', completion_location: completion() }); await approval;
  assert.equal(f.navigations.length, 0);
  assert.equal(f.element('device-details').hidden, true);
  assert.equal(f.text().includes(code), false);
});

test('older session response cannot restore the previous account proof', async () => {
  const f = fixture(); const refresh = f.element('device-refresh').fire('click');
  f.reply(1, 200, session('user-b', 'private-csrf-b')); await flush();
  f.reply(2, 200, enrollment()); await refresh;
  f.reply(0, 200, session()); await flush();
  const denial = f.element('device-deny').fire('click');
  assert.equal(f.requests[3].options.headers['X-PairRoom-CSRF'], 'private-csrf-b');
  f.reply(3, 200, { request_id: 'request-1', status: 'denied' }); await denial;
  assert.equal(f.navigations.length, 0);
});

test('uncertain decision never retries automatically and reapproval requires an explicit new action', async () => {
  const f = await ready(); const first = f.element('device-approve').fire('click');
  const firstID = JSON.parse(f.requests[2].options.body).client_id;
  f.reply(2, 503, { outcome: 'unknown' }); await first;
  assert.equal(f.requests.length, 3);
  await f.element('device-approve').fire('click'); assert.equal(f.requests.length, 3);
  const refresh = f.element('device-refresh').fire('click');
  f.reply(3, 200, session()); await flush(); f.reply(4, 200, enrollment('My laptop', 'approved')); await refresh;
  assert.equal(f.element('device-approve').textContent, 'Approve again');
  const second = f.element('device-approve').fire('click');
  assert.notEqual(JSON.parse(f.requests[5].options.body).client_id, firstID);
  f.reply(5, 200, { request_id: 'request-1', completion_location: completion() }); await second;
});

test('unsafe completion URLs and mismatched request receipts never navigate', async () => {
  for (const result of [
    { request_id: 'request-1', completion_location: 'https://attacker.example.test/?token=private' },
    { request_id: 'request-1', completion_location: completion().replace('127.0.0.1', '192.168.0.1') },
    { request_id: 'request-1', completion_location: completion().replace(':49152', ':0') },
    { request_id: 'different-request', completion_location: completion() }
  ]) {
    const f = await ready(); const approval = f.element('device-approve').fire('click'); f.reply(2, 200, result); await approval;
    assert.equal(f.navigations.length, 0);
  }
  const f = await ready(); const approval = f.element('device-approve').fire('click'); f.reply(2, 200, { request_id: 'request-1', completion_location: completion(80) }); await approval;
  assert.deepEqual(f.navigations, [completion(80)]);
});

test('untrusted device metadata is displayed as text and cleared before refresh', async () => {
  const f = fixture(); f.reply(0, 200, session()); await flush();
  const label = '<img src=x onerror=steal()> device'; f.reply(1, 200, enrollment(label)); await flush();
  assert.equal(f.element('device-label').textContent, label);
  const refresh = f.element('device-refresh').fire('click');
  assert.equal(f.element('device-label').textContent, ''); assert.equal(f.element('device-origin').textContent, '');
  f.reply(2, 401, {}); await refresh;
});

test('another tab changing the account clears an idle approval and rejects its late completion', async () => {
  const f = await ready(); const approval = f.element('device-approve').fire('click');
  f.broadcast({ deployment: 'deployment-1', user: 'user-b', session: 'session-b' });
  assert.equal(f.element('device-details').hidden, true); assert.equal(f.element('device-label').textContent, '');
  f.reply(2, 200, { request_id: 'request-1', completion_location: completion() }); await approval;
  assert.equal(f.navigations.length, 0); assert.equal(f.requests.length, 3);
});
