'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const source = fs.readFileSync(path.join(__dirname, '../internal/enterpriseservice/web/app.js'), 'utf8');
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const flush = () => new Promise((resolve) => setImmediate(resolve));

class Element {
  constructor() { this.listeners = new Map(); this.children = []; this.textContent = ''; this.hidden = false; this.disabled = false; this.value = ''; }
  addEventListener(type, fn) { this.listeners.set(type, fn); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  fire(type) { return this.listeners.get(type)({ preventDefault() {} }); }
  text() { return this.textContent + this.children.map((child) => child.text()).join(''); }
}

function fixture() {
  const elements = new Map();
  const element = (id) => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const requests = [];
  let nextID = 0;
  const context = vm.createContext({
    document: { getElementById: element, createElement: () => new Element() },
    AbortController,
    crypto: { randomUUID: () => `client-${++nextID}` },
    // Deliberately ignore AbortSignal. A late transport or JSON response must
    // still be discarded by account/request generation, not merely aborted.
    fetch: (url, options) => { const result = deferred(); requests.push({ url, options, ...result }); return result.promise; }
  });
  vm.runInContext(source, context);
  const reply = (index, status, payload) => requests[index].resolve({ status, ok: status >= 200 && status < 300, json: async () => payload });
  return { element, requests, reply, text: () => [...elements.values()].map((value) => value.text()).join('') };
}

const session = (user = 'user-a', csrf = 'private-csrf-a') => ({ user_id: user, csrf, administrator: false });
const room = (id = 'room-a', text = 'private-room-a') => ({ room_id: id, messages: [{ from: 'user', to: 'slot1', state: 'queued', text }] });
async function signedIn() {
  const f = fixture();
  f.reply(0, 200, session());
  await flush();
  assert.equal(f.element('open-room').disabled, false);
  return f;
}

test('logout immediately clears and invalidates a pending private Room response', async () => {
  const f = await signedIn();
  f.element('room-id').value = 'room-a';
  const reading = f.element('room-form').fire('submit');
  const logout = f.element('sign-out').fire('click');
  assert.equal(f.requests[1].options.signal.aborted, true);
  assert.equal(f.element('room').hidden, true);
  assert.equal(f.element('open-room').disabled, true);
  f.reply(2, 200, { outcome: 'committed' });
  await logout;
  f.reply(1, 200, room());
  await reading;
  assert.equal(f.element('room').hidden, true);
  assert.equal(f.element('messages').children.length, 0);
  assert.equal(f.text().includes('private-room-a'), false);
  assert.equal(f.text().includes('private-csrf-a'), false);
});

test('account refresh clears existing DOM and discards an old Room JSON body after account switch', async () => {
  const f = await signedIn();
  f.element('room-id').value = 'room-a';
  const reading = f.element('room-form').fire('submit');
  const oldJSON = deferred();
  f.requests[1].resolve({ status: 200, ok: true, json: () => oldJSON.promise });
  await flush();
  f.element('messages').append(Object.assign(new Element(), { textContent: 'previously-rendered-private' }));
  const refresh = f.element('refresh').fire('click');
  assert.equal(f.element('messages').children.length, 0);
  assert.equal(f.requests[1].options.signal.aborted, true);
  f.reply(2, 200, session('user-b', 'private-csrf-b'));
  await refresh;
  oldJSON.resolve(room());
  await reading;
  assert.equal(f.element('room').hidden, true);
  assert.equal(f.text().includes('private-room-a'), false);
  assert.equal(f.text().includes('previously-rendered-private'), false);
  assert.equal(f.element('open-room').disabled, false);
});

for (const sameUser of [false, true]) {
  test(`older session refresh cannot replace the newest ${sameUser ? 'same-user session' : 'account'}`, async () => {
    const f = await signedIn();
    const older = f.element('refresh').fire('click');
    const newer = f.element('refresh').fire('click');
    assert.equal(f.requests[1].options.signal.aborted, true);
    f.reply(2, 200, session(sameUser ? 'user-a' : 'user-b', 'private-csrf-new'));
    await newer;
    f.reply(1, 200, session());
    await older;
    const logout = f.element('sign-out').fire('click');
    assert.equal(f.requests[3].options.headers['X-PairRoom-CSRF'], 'private-csrf-new');
    assert.equal(f.text().includes('private-csrf-new'), false);
    f.reply(3, 200, { outcome: 'committed' });
    await logout;
  });
}

test('late logout response cannot sign out a subsequently refreshed account', async () => {
  const f = await signedIn();
  const logout = f.element('sign-out').fire('click');
  const refresh = f.element('refresh').fire('click');
  assert.equal(f.requests[1].options.signal.aborted, true);
  f.reply(2, 200, session('user-b', 'private-csrf-b'));
  await refresh;
  f.reply(1, 200, { outcome: 'committed' });
  await logout;
  assert.equal(f.element('sign-in').hidden, true);
  assert.equal(f.element('sign-out').hidden, false);
  assert.equal(f.element('open-room').disabled, false);
  f.element('room-id').value = 'room-b';
  const reading = f.element('room-form').fire('submit');
  f.reply(3, 200, room('room-b', 'current-account-body'));
  await reading;
  assert.equal(f.element('room-title').textContent, 'room-b');
  assert.equal(f.element('messages').text().includes('current-account-body'), true);
});

test('late session refresh cannot restore credentials or administrator state after logout', async () => {
  const f = await signedIn();
  const older = f.element('refresh').fire('click');
  const current = f.element('refresh').fire('click');
  f.reply(2, 200, session());
  await current;
  const logout = f.element('sign-out').fire('click');
  f.reply(3, 200, { outcome: 'committed' });
  await logout;
  f.reply(1, 200, { ...session(), administrator: true });
  await older;
  assert.equal(f.element('sign-in').hidden, false);
  assert.equal(f.element('sign-out').hidden, true);
  assert.equal(f.element('open-room').disabled, true);
  assert.equal(f.text().includes('administrator'), false);
  assert.equal(f.text().includes('private-csrf-a'), false);
  const count = f.requests.length;
  await f.element('sign-out').fire('click');
  assert.equal(f.requests.length, count);
});

test('account invalidation removes rendered Room metadata and input as well as messages', async () => {
  const f = await signedIn();
  f.element('room-id').value = 'room-private';
  const reading = f.element('room-form').fire('submit');
  f.reply(1, 200, room('room-private', 'private-rendered-body'));
  await reading;
  assert.equal(f.element('room-title').textContent, 'room-private');
  const refresh = f.element('refresh').fire('click');
  assert.equal(f.element('room-title').textContent, 'Room');
  assert.equal(f.element('room-sequence').textContent, '');
  assert.equal(f.element('room-id').value, '');
  assert.equal(f.text().includes('private-rendered-body'), false);
  f.reply(2, 401, {});
  await refresh;
});

test('latest Room selection wins even when the previous response ignores cancellation', async () => {
  const f = await signedIn();
  f.element('room-id').value = 'room-a';
  const first = f.element('room-form').fire('submit');
  f.element('room-id').value = 'room-b';
  const second = f.element('room-form').fire('submit');
  assert.equal(f.requests[1].options.signal.aborted, true);
  f.reply(2, 200, room('room-b', 'current-room-body'));
  await second;
  f.reply(1, 200, room());
  await first;
  assert.equal(f.element('room-title').textContent, 'room-b');
  assert.equal(f.element('messages').text().includes('private-room-a'), false);
});

test('unknown logout never retries automatically and retains its operation only for the same session proof', async () => {
  const f = await signedIn();
  const first = f.element('sign-out').fire('click');
  const firstID = JSON.parse(f.requests[1].options.body).client_id;
  f.reply(1, 503, { outcome: 'unknown' });
  await first;
  assert.equal(f.requests.length, 2);
  const refresh = f.element('refresh').fire('click');
  f.reply(2, 200, session());
  await refresh;
  const retry = f.element('sign-out').fire('click');
  assert.equal(JSON.parse(f.requests[3].options.body).client_id, firstID);
  f.reply(3, 503, { outcome: 'unknown' });
  await retry;
  const changed = f.element('refresh').fire('click');
  f.reply(4, 200, session('user-b', 'private-csrf-b'));
  await changed;
  const next = f.element('sign-out').fire('click');
  assert.notEqual(JSON.parse(f.requests[5].options.body).client_id, firstID);
  assert.equal(f.requests[5].options.headers['X-PairRoom-CSRF'], 'private-csrf-b');
  f.reply(5, 200, { outcome: 'committed' });
  await next;
});
