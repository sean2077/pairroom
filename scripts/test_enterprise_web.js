'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { Element, fixture, signIn, session, history, headers, open, deferred, flush } = require('./enterprise_web_test_helpers');

test('logout clears and invalidates pending private Room responses immediately', async () => {
  const f = await signIn(); f.element('room-id').value = 'room-a';
  const reading = f.element('room-form').fire('submit'); await flush();
  const old = f.take('/rooms/room-a'), logout = f.element('sign-out').fire('click');
  assert.equal(old.options.signal.aborted, true); assert.equal(f.element('workspace').hidden, true);
  f.reply('/session/logout', 200, { outcome: 'committed' }); await logout;
  f.complete(old, 200, history()); await reading;
  assert.equal(f.element('messages').children.length, 0); assert.equal(f.text().includes('private-room-a'), false);
});

test('account refresh removes rendered content and discards old Room JSON', async () => {
  const f = await signIn(); f.element('room-id').value = 'room-a';
  const reading = f.element('room-form').fire('submit'); await flush();
  const old = f.take('/rooms/room-a'), json = deferred(); old.resolved = true;
  old.resolve({ status: 200, ok: true, headers: headers(session()), json: () => json.promise }); await flush();
  f.element('messages').append(Object.assign(new Element(), { textContent: 'rendered-private' }));
  const refresh = f.element('refresh').fire('click'); assert.equal(f.element('messages').children.length, 0);
  await signIn(f, session('user-b', 'session-b')); await refresh; json.resolve(history()); await reading;
  assert.equal(f.text().includes('rendered-private'), false); assert.equal(f.text().includes('private-room-a'), false);
});

for (const sameUser of [false, true]) test('older session refresh cannot replace the latest ' + (sameUser ? 'same-user session' : 'account'), async () => {
  const f = await signIn(); const oldRefresh = f.element('refresh').fire('click'), old = f.take('/session');
  const newest = f.element('refresh').fire('click'), latest = f.requests.filter((value) => value.url.endsWith('/session') && !value.resolved).at(-1);
  const changed = session(sameUser ? 'user-a' : 'user-b', 'session-new'); f.setIdentity(changed); f.complete(latest, 200, changed, changed); await newest;
  f.complete(old, 200, session(), session()); await oldRefresh;
  const logout = f.element('sign-out').fire('click'); assert.equal(f.take('/session/logout').options.headers['X-PairRoom-CSRF'], changed.csrf);
  f.reply('/session/logout', 200, { outcome: 'committed' }); await logout;
});

test('late logout response cannot sign out a subsequently refreshed account', async () => {
  const f = await signIn(); const logout = f.element('sign-out').fire('click'), old = f.take('/session/logout');
  const refresh = f.element('refresh').fire('click'); await signIn(f, session('user-b', 'session-b')); await refresh;
  f.complete(old, 200, { outcome: 'committed' }, session()); await logout;
  assert.equal(f.element('sign-in').hidden, true); await open(f, 'room-b', 'current-body');
  assert.equal(f.element('messages').text().includes('current-body'), true);
});

test('late session results cannot restore authority after logout', async () => {
  const f = await signIn(); const oldRefresh = f.element('refresh').fire('click'), old = f.take('/session');
  const newest = f.element('refresh').fire('click'), latest = f.requests.filter((value) => value.url.endsWith('/session') && !value.resolved).at(-1);
  f.complete(latest, 200, session(), session()); await newest;
  const logout = f.element('sign-out').fire('click'); f.reply('/session/logout', 200, { outcome: 'committed' }); await logout;
  f.complete(old, 200, { ...session(), administrator: true }, session()); await oldRefresh;
  assert.equal(f.element('workspace').hidden, true); assert.equal(f.text().includes('administrator'), false);
});

test('account invalidation clears drafts, private lists, metadata and pending views', async () => {
  const f = await signIn(); await open(f);
  f.element('message-text').value = 'private draft'; f.element('create-project-name').value = 'private project';
  const refresh = f.element('refresh').fire('click');
  assert.equal(f.element('room-title').textContent, 'Room'); assert.equal(f.element('message-text').value, '');
  assert.equal(f.element('create-project-name').value, ''); assert.equal(f.element('room-id').value, '');
  assert.equal(f.element('room-list').children.length, 0); f.reply('/session', 401, {}); await refresh;
});

test('latest Room selection wins if previous fetch ignores cancellation', async () => {
  const f = await signIn(); f.element('room-id').value = 'room-a';
  const first = f.element('room-form').fire('submit'); await flush(); const old = f.take('/rooms/room-a');
  f.element('room-id').value = 'room-b'; const second = f.element('room-form').fire('submit'); await flush();
  f.reply('/rooms/room-b', 200, history('room-b', 'current-room')); await second;
  f.complete(old, 200, history('room-a', 'stale-room')); await first;
  assert.equal(f.element('room-title').textContent, 'room-b'); assert.equal(f.text().includes('stale-room'), false);
});

test('unknown logout is never retried automatically and retains original id only for the same session', async () => {
  const f = await signIn(); const first = f.element('sign-out').fire('click'), request = f.take('/session/logout');
  const original = JSON.parse(request.options.body).client_id; f.complete(request, 503, { outcome: 'unknown' }); await first;
  assert.equal(f.requests.filter((value) => value.url.endsWith('/session/logout')).length, 1);
  const refresh = f.element('refresh').fire('click'); await signIn(f); await refresh;
  const retry = f.element('sign-out').fire('click'); assert.equal(JSON.parse(f.take('/session/logout').options.body).client_id, original);
  f.reply('/session/logout', 200, { outcome: 'committed' }); await retry;
});
