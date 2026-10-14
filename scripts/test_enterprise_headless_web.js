'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../internal/enterpriseservice/web/device.js'), 'utf8');
const flush = () => new Promise(resolve => setImmediate(resolve));
const requestID = 'request-' + 'a'.repeat(32);
class Element {
  constructor() { this.value = ''; this.textContent = ''; this.hidden = false; this.disabled = false; this.listeners = new Map(); }
  addEventListener(type, handler) { this.listeners.set(type, handler); }
  fire(type) { return this.listeners.get(type)(); }
  set innerHTML(_value) { throw new Error('metadata must be text'); }
}
function fixture() {
  const elements = new Map(), requests = [], navigations = []; let identity = { user_id: 'user-a', session_id: 'session-a', deployment_id: 'deployment-a', csrf: 'private-csrf' }, next = 0, channel;
  const element = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const globals = { document: { getElementById: element }, window: { location: { pathname: '/enterprise/device-enrollments/' + requestID, assign: value => navigations.push(value) }, localStorage: { setItem() { throw new Error('short code must not be persisted'); } } }, URL, Date, AbortController, crypto: { randomUUID: () => 'decision-' + (++next) }, BroadcastChannel: class { constructor() { channel = this; } }, fetch: (url, options) => new Promise((resolve, reject) => requests.push({ url, options, resolve, reject })) };
  vm.runInNewContext(source, globals);
  const reply = (index, status, data) => { if (requests[index].url.endsWith('/session') && data.user_id) identity = data; requests[index].resolve({ status, ok: status >= 200 && status < 300, headers: { get: name => ({ 'X-PairRoom-User': identity.user_id, 'X-PairRoom-Session': identity.session_id })[name] || null }, json: async () => data }); };
  return { element, requests, navigations, reply, identity: () => identity, broadcast: data => channel.onmessage({ data }) };
}
async function ready() {
  const f = fixture(); f.reply(0, 200, f.identity()); await flush();
  f.reply(1, 200, { request_id: requestID, device_id: 'device-a', origin: 'https://service.example.test', label: 'My server', transport_mode: 'headless', candidate_fingerprint: '1'.repeat(32), status: 'pending', expires_at: '2030-01-01T00:00:00Z' }); await flush(); return f;
}
function result(status = 'approved', disposition = '') { return { outcome: 'committed', kind: 'device.enrollment.headless.decide', transport_mode: 'headless', request_id: requestID, status, ...(disposition ? { disposition } : {}) }; }

test('headless metadata never approves automatically and only accepts the terminal uppercase code format', async () => {
  const f = await ready(); assert.equal(f.element('device-confirmation').hidden, false); assert.equal(f.requests.length, 2);
  for (const code of ['', 'abcd-efgh', 'ABCD-I123', 'ABCD O123', ' ABCD-EFGH', 'ABCD-EFGH\n', 'ＡＢＣＤＥＦＧＨ']) { f.element('device-code').value = code; await f.element('device-approve').fire('click'); assert.equal(f.requests.length, 2); }
  assert.equal(f.navigations.length, 0);
});

test('headless explicit confirmation uses current CSRF once and returns to the original CLI without navigation', async () => {
  const f = await ready(); f.element('device-code').value = 'ABCD-EFGH'; const approval = f.element('device-approve').fire('click');
  assert.equal(JSON.parse(f.requests[2].options.body).confirmation_code, 'ABCD-EFGH'); assert.equal(f.requests[2].options.headers['X-PairRoom-CSRF'], 'private-csrf'); assert.equal(f.element('device-code').value, '');
  f.reply(2, 200, result()); await approval;
  assert.match(f.element('device-status').textContent, /Device approved.*original CLI.*Room membership/); assert.equal(f.navigations.length, 0); assert.equal(f.requests.length, 3);
});

test('committed counter rejection and exhaustion are explained without treating them as approval or uncertainty', async () => {
  for (const [httpStatus, disposition, expected] of [[400, 'confirmation_rejected', /failed confirmation was recorded/], [429, 'verification_exhausted', /request was invalidated/]]) {
    const f = await ready(); f.element('device-code').value = 'ABCD-EFGH'; const approval = f.element('device-approve').fire('click');
    f.reply(2, httpStatus, result(disposition === 'confirmation_rejected' ? 'pending' : 'invalidated', disposition)); await approval;
    assert.match(f.element('device-status').textContent, expected); assert.equal(f.element('device-code').value, ''); assert.equal(f.navigations.length, 0);
    await f.element('device-approve').fire('click'); assert.equal(f.requests.length, 3, 'no automatic retry after committed counter result');
  }
});

test('headless unknown response never retries or navigates and a changed account discards the input', async () => {
  const f = await ready(); f.element('device-code').value = 'ABCD-EFGH'; const approval = f.element('device-approve').fire('click');
  f.reply(2, 503, { outcome: 'unknown' }); await approval; assert.equal(f.requests.length, 3); assert.match(f.element('device-status').textContent, /could not be confirmed/); assert.equal(f.navigations.length, 0);
  f.element('device-code').value = 'ABCD-EFGH'; f.broadcast({ deployment: 'deployment-a', user: 'user-b', session: 'session-b' });
  assert.equal(f.element('device-code').value, ''); assert.equal(f.element('device-details').hidden, true); assert.equal(f.element('device-fingerprint').textContent, '');
});

test('late headless approval cannot confirm for another account and no completion redirect is accepted', async () => {
  const f = await ready(); f.element('device-code').value = 'ABCD-EFGH'; const approval = f.element('device-approve').fire('click');
  f.broadcast({ deployment: 'deployment-a', user: 'user-b', session: 'session-b' }); f.reply(2, 200, result()); await approval;
  assert.equal(f.navigations.length, 0); assert.equal(f.element('device-status').textContent, '');
  const unsafe = await ready(); unsafe.element('device-code').value = 'ABCD-EFGH'; const sending = unsafe.element('device-approve').fire('click'); unsafe.reply(2, 200, { ...result(), completion_location: 'http://127.0.0.1:9000/callback?token=private' }); await sending;
  assert.match(unsafe.element('device-status').textContent, /could not be confirmed/); assert.equal(unsafe.navigations.length, 0);
});
