'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');
const source = fs.readFileSync(path.join(__dirname, '../internal/enterpriseservice/web/binding.js'), 'utf8');
const flush = () => new Promise((resolve) => setImmediate(resolve));
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

class Element {
  constructor() { this.listeners = new Map(); this.textContent = ''; this.hidden = false; this.disabled = false; }
  addEventListener(type, listener) { this.listeners.set(type, listener); }
  fire(type) { return this.listeners.get(type)(); }
  set innerHTML(_value) { throw new Error('Binding metadata must render as text'); }
}

function fixture() {
  const elements = new Map(), requests = [];
  const element = (id) => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  let nextID = 0;
  let currentSession = session(), channel;
  const responseHeaders = (value = currentSession) => ({ get: (name) => ({ 'X-PairRoom-User': value.user_id, 'X-PairRoom-Session': value.session_id })[name] || null });
  vm.runInNewContext(source, {
    document: { getElementById: element },
    window: { location: { pathname: '/enterprise/binding-requests/room-1/request-1' } },
    AbortController, Date,
    BroadcastChannel: class { constructor() { channel = this; } },
    crypto: { randomUUID: () => `operation-${++nextID}` },
    // The transport ignores AbortSignal so stale fetch and JSON responses must
    // also be rejected by the page's account generation checks.
    fetch: (url, options) => { const value = deferred(); requests.push({ url, options, ...value }); return value.promise; }
  });
  const reply = (index, status, payload) => { if (requests[index].url.endsWith('/session') && payload.user_id) currentSession = payload; requests[index].resolve({ status, ok: status >= 200 && status < 300, headers: responseHeaders(), json: async () => payload }); };
  return { element, requests, reply, responseHeaders, broadcast: (data) => channel.onmessage({ data }), text: () => [...elements.values()].map((value) => value.textContent).join('') };
}

const session = (user = 'owner-1', csrf = 'private-csrf-1') => ({ user_id: user, csrf, session_id: 'session-' + csrf, deployment_id: 'deployment-1' });
const info = (changes = {}) => ({ request_id: 'request-1', room_id: 'room-1', user_id: 'member-1', device_id: 'device-1', agent_id: 'agent-1', runtime: 'codex', slot: 'slot1', observed_generation: 3, replace: true, park_enabled: false, status: 'pending', expires_at: '2030-01-01T00:00:00Z', ...changes });
const context = (changes = {}) => ({ room_id: 'room-1', owner_user_id: 'owner-1', ownership_pending: false, slots: { slot1: { head_generation: 3 }, slot2: { head_generation: 2 } }, ...changes });
const receipt = (action = 'approve', changes = {}) => ({ outcome: 'committed', receipt: { kind: 'decide', request: info({ status: action === 'approve' ? 'approved' : 'denied', ...changes }) } });
async function metadata(f, first, value = info(), current = context()) {
  f.reply(first, 200, { requests: [value] }); await flush();
  f.reply(first + 1, 200, { request: value }); await flush();
  f.reply(first + 2, 200, current); await flush();
}
async function ready(value = info(), current = context()) { const f = fixture(); f.reply(0, 200, session()); await flush(); await metadata(f, 1, value, current); return f; }

test('Owner review loads only safe request metadata and requires an explicit decision', async () => {
  const f = await ready();
  assert.deepEqual(f.requests.map((r) => r.url), ['/enterprise/api/v1/session', '/enterprise/api/v1/rooms/room-1/binding-requests', '/enterprise/api/v1/rooms/room-1/binding-requests/request-1', '/enterprise/api/v1/rooms/room-1/binding-context']);
  assert.equal(f.element('binding-approve').disabled, false);
  assert.equal(f.element('binding-runtime').textContent, 'codex');
  assert.match(f.element('binding-replacement').textContent, /replaces the existing Binding/);
  assert.equal(f.element('binding-park').textContent, 'Disabled by the endpoint');
  assert.equal(f.text().includes('private-csrf'), false);
  const unsigned = fixture(); unsigned.reply(0, 401, {}); await flush();
  await unsigned.element('binding-approve').fire('click');
  assert.equal(unsigned.requests.length, 1);
  assert.equal(unsigned.element('binding-sign-in').hidden, false);
});

test('approval sends only the explicit action and current CSRF, never endpoint completion', async () => {
  const f = await ready(); const action = f.element('binding-approve').fire('click');
  const request = f.requests[4];
  assert.equal(request.url, '/enterprise/api/v1/rooms/room-1/binding-requests/request-1/decision');
  assert.equal(request.options.method, 'POST');
  assert.equal(request.options.redirect, 'error');
  assert.equal(request.options.headers['X-PairRoom-CSRF'], 'private-csrf-1');
  assert.deepEqual(JSON.parse(request.options.body), { client_id: 'operation-1', action: 'approve' });
  f.reply(4, 200, receipt()); await action;
  assert.match(f.element('binding-status').textContent, /original endpoint must now complete/);
  assert.equal(f.requests.length, 5);
  assert.equal(f.requests.some((r) => r.url.includes('/device/') || r.url.endsWith('/complete')), false);
});

test('changed slot generation, governance recovery and unsafe integer heads disable approval', async () => {
  for (const [value, current] of [
    [info(), context({ slots: { slot1: { head_generation: 4 } } })],
    [info(), context({ ownership_pending: true })],
    [info({ observed_generation: 9007199254740992 }), context({ slots: { slot1: { head_generation: 9007199254740992 } } })]
  ]) {
    const f = await ready(value, current);
    assert.equal(f.element('binding-approve').disabled, true);
    await f.element('binding-approve').fire('click');
    assert.equal(f.requests.length, 4);
  }
});

test('an administrator flag cannot substitute for the current Room Owner', async () => {
  const f = fixture(); f.reply(0, 200, { ...session('administrator'), administrator: true }); await flush();
  f.reply(1, 403, {}); await flush();
  assert.equal(f.element('binding-details').hidden, true);
  assert.equal(f.requests.length, 2);
  const mismatched = await ready(info(), context({ owner_user_id: 'someone-else' }));
  assert.equal(mismatched.element('binding-details').hidden, true);
  await mismatched.element('binding-approve').fire('click'); assert.equal(mismatched.requests.length, 4);
});

test('late request JSON cannot restore private metadata after account refresh', async () => {
  const f = fixture(); f.reply(0, 200, session()); await flush(); f.reply(1, 200, { requests: [] }); await flush();
  const oldJSON = deferred(); f.requests[2].resolve({ status: 200, ok: true, headers: f.responseHeaders(), json: () => oldJSON.promise }); await flush();
  const refresh = f.element('binding-refresh').fire('click');
  assert.equal(f.requests[2].options.signal.aborted, true);
  f.reply(3, 200, session('other-owner', 'other-csrf')); await flush();
  await metadata(f, 4, info({ agent_id: 'new-account-agent' }), context({ owner_user_id: 'other-owner' })); await refresh;
  oldJSON.resolve({ request: info({ agent_id: 'old-private-agent' }) }); await flush();
  assert.equal(f.element('binding-agent').textContent, 'new-account-agent');
  assert.equal(f.text().includes('old-private-agent'), false);
  assert.equal(f.requests.length, 7);
});

test('an old session response cannot restore its identity or mutation proof', async () => {
  const f = fixture(); const refresh = f.element('binding-refresh').fire('click');
  f.reply(1, 200, session('new-owner', 'new-csrf')); await flush();
  await metadata(f, 2, info(), context({ owner_user_id: 'new-owner' })); await refresh;
  f.reply(0, 200, session()); await flush();
  const action = f.element('binding-deny').fire('click');
  assert.equal(f.requests[5].options.headers['X-PairRoom-CSRF'], 'new-csrf');
  f.reply(5, 200, receipt('deny')); await action;
  assert.equal(f.element('binding-account').textContent, 'Account: new-owner');
});

test('late decision JSON is discarded when session refresh invalidates the account', async () => {
  const f = await ready(); const action = f.element('binding-approve').fire('click');
  const oldJSON = deferred(); f.requests[4].resolve({ status: 200, ok: true, headers: f.responseHeaders(), json: () => oldJSON.promise }); await flush();
  const refresh = f.element('binding-refresh').fire('click');
  f.reply(5, 401, {}); await refresh;
  oldJSON.resolve(receipt()); await action;
  assert.equal(f.element('binding-details').hidden, true);
  assert.equal(f.element('binding-status').textContent, '');
  assert.equal(f.element('binding-resolve').hidden, true);
  await f.element('binding-resolve').fire('click'); assert.equal(f.requests.length, 6);
});

test('unknown decision needs explicit original-operation resolution with no effect retry', async () => {
  const f = await ready(); const action = f.element('binding-approve').fire('click');
  const original = f.requests[4].options.body;
  f.reply(4, 503, { outcome: 'unknown' }); await action;
  assert.equal(f.requests.length, 5);
  await f.element('binding-approve').fire('click'); assert.equal(f.requests.length, 5);
  const check = f.element('binding-resolve').fire('click');
  assert.equal(f.requests[5].url, '/enterprise/api/v1/rooms/room-1/binding-requests/request-1/resolve-decision');
  assert.equal(f.requests[5].options.body, original);
  f.reply(5, 200, { ...receipt(), outcome: 'replayed' }); await check;
  assert.match(f.element('binding-status').textContent, /^Approved/);
  assert.equal(f.requests.filter((r) => r.url.endsWith('/decision')).length, 1);
});

test('known not-committed resolution permits only a fresh explicit decision after refresh', async () => {
  const f = await ready(); const action = f.element('binding-deny').fire('click');
  f.reply(4, 503, { outcome: 'unknown' }); await action;
  const check = f.element('binding-resolve').fire('click'); f.reply(5, 409, { outcome: 'not_committed' }); await check;
  await f.element('binding-deny').fire('click'); assert.equal(f.requests.length, 6);
  const refresh = f.element('binding-refresh').fire('click'); f.reply(6, 200, session()); await flush(); await metadata(f, 7); await refresh;
  const next = f.element('binding-deny').fire('click');
  assert.notEqual(JSON.parse(f.requests[10].options.body).client_id, JSON.parse(f.requests[4].options.body).client_id);
  f.reply(10, 200, receipt('deny')); await next;
});

test('account changes discard a pending decision even when the old account remains a Room Owner', async () => {
  const f = await ready(); const action = f.element('binding-approve').fire('click'); f.reply(4, 503, { outcome: 'unknown' }); await action;
  const refresh = f.element('binding-refresh').fire('click'); f.reply(5, 200, session('owner-1', 'new-session-csrf')); await flush(); await metadata(f, 6); await refresh;
  assert.equal(f.element('binding-resolve').hidden, true);
  await f.element('binding-resolve').fire('click'); assert.equal(f.requests.length, 9);
});

test('untrusted metadata is text-only and mismatched receipts never claim approval', async () => {
  const untrusted = '<img src=x onerror=steal()> agent';
  const f = await ready(info({ agent_id: untrusted }));
  assert.equal(f.element('binding-agent').textContent, untrusted);
  const action = f.element('binding-approve').fire('click');
  assert.equal(f.element('binding-agent').textContent, '');
  f.reply(4, 200, receipt('approve', { room_id: 'different-room' })); await action;
  assert.match(f.element('binding-status').textContent, /could not be confirmed/);
  assert.equal(f.text().includes('private-csrf'), false);
});

test('another tab logout removes Owner metadata and original decision controls', async () => {
  const f = await ready(); const approval = f.element('binding-approve').fire('click');
  f.broadcast({ deployment: 'deployment-1', user: '', session: '' });
  assert.equal(f.element('binding-details').hidden, true); assert.equal(f.element('binding-user').textContent, '');
  f.reply(4, 200, receipt()); await approval;
  assert.equal(f.element('binding-status').textContent, ''); assert.equal(f.element('binding-resolve').hidden, true);
});
