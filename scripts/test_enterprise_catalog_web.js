'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { fixture, signIn, session, history, headers, open, deferred, flush, Hub, Storage } = require('./enterprise_web_test_helpers');

test('two tabs discard private replies and drafts on another tab account change without broadcast echo', async () => {
  const hub = new Hub(), storage = new Storage();
  const a = await signIn(fixture({ hub, storage })), b = await signIn(fixture({ hub, storage })); await flush();
  a.element('room-id').value = 'room-a'; const reading = a.element('room-form').fire('submit'); await flush();
  const old = a.take('/rooms/room-a'), oldJSON = deferred(); old.resolved = true;
  old.resolve({ status: 200, ok: true, headers: headers(session()), json: () => oldJSON.promise }); await flush();
  a.element('message-text').value = 'private draft';
  const refresh = b.element('refresh').fire('click'); await signIn(b, session('user-b', 'session-b')); await refresh; await flush();
  assert.equal(a.element('message-text').value, ''); assert.equal(a.element('workspace').hidden, true);
  assert.equal(a.requests.filter((r) => !r.resolved && r.url.endsWith('/session')).length, 1);
  await signIn(a, session('user-b', 'session-b')); oldJSON.resolve(history()); await reading; await flush();
  assert.equal(a.text().includes('private-room-a'), false);
  assert.equal(hub.sent.length, 3); // two initial announcements and one change, no echo.
  assert.equal(JSON.stringify(hub.sent).includes('csrf'), false);
});

test('another tab logout clears an idle rendered Room and pending controls immediately', async () => {
  const hub = new Hub(); const a = await signIn(fixture({ hub })), b = await signIn(fixture({ hub })); await flush();
  await open(a); a.element('message-text').value = 'unsent secret';
  const logout = b.element('sign-out').fire('click'); await flush();
  assert.equal(a.element('workspace').hidden, true); assert.equal(a.element('messages').children.length, 0); assert.equal(a.element('message-text').value, '');
  b.reply('/session/logout', 200, { outcome: 'committed' }); await logout;
  assert.equal(a.requests.filter((r) => !r.resolved && r.url.endsWith('/session')).length, 0);
});

test('a successful API reply for another session is rejected before JSON and triggers bounded auth refresh', async () => {
  const f = await signIn(); f.element('room-id').value = 'room-a'; const reading = f.element('room-form').fire('submit'); await flush();
  const request = f.take('/rooms/room-a'); let bodyReads = 0; request.resolved = true;
  request.resolve({ status: 200, ok: true, headers: headers(session('user-a', 'new-session')), json: async () => { bodyReads += 1; return history(); } });
  await reading; await flush(); assert.equal(bodyReads, 0); assert.equal(f.element('workspace').hidden, true);
  assert.equal(f.requests.filter((r) => !r.resolved && r.url.endsWith('/session')).length, 1);
  f.reply('/session', 401, {}); await flush();
});

test('Room creation uses server-frozen collaboration with no Runtime selection or automatic retry', async () => {
  const f = await signIn(); f.element('create-room-name').value = 'Research';
  const creation = f.element('create-room-form').fire('submit');
  const template = f.take('/room-template'); assert.deepEqual(JSON.parse(template.options.body), { name: 'Research', collaboration: { mode: 'default' } });
  const intent = { name: 'Research', collaboration: { version: 2, mode: 'default', instructions: 'Server-frozen instructions' } };
  f.complete(template, 200, { intent }); await flush(); const command = f.take('/rooms'), body = JSON.parse(command.options.body);
  assert.deepEqual(body.intent, intent); assert.equal('runtime' in body, false); assert.equal('provider' in body, false);
  f.complete(command, 503, { outcome: 'unknown' }); await creation;
  assert.equal(f.requests.filter((r) => r.url.endsWith('/rooms') && r.options.method === 'POST').length, 1);
  assert.equal(f.element('pending').hidden, false); assert.equal(f.storage.length, 1);
  assert.equal([...f.storage.values.values()][0].includes('private-csrf'), false);
});

async function uncertainMessage(f) {
  await open(f); f.element('message-text').value = 'private pending message';
  const send = f.element('send-form').fire('submit'), request = f.take('/rooms/room-a/messages');
  const body = JSON.parse(request.options.body); f.complete(request, 503, { outcome: 'unknown' }); await send; return body;
}

test('reload recovers only the current User journal and resolves the original message without resending', async () => {
  const storage = new Storage(), f = await signIn(fixture({ storage })), original = await uncertainMessage(f);
  const other = await signIn(fixture({ storage }), session('user-b', 'session-b'));
  assert.equal(other.element('pending').hidden, true); assert.equal(other.text().includes('private pending message'), false);
  const reloaded = await signIn(fixture({ storage }), session('user-a', 'same-user-new-session'));
  assert.equal(reloaded.element('pending').hidden, false);
  assert.equal(reloaded.requests.filter((r) => r.options.method === 'POST').length, 0);
  const resolve = reloaded.element('resolve-operation').fire('click'), request = reloaded.take('/rooms/room-a/resolve-message');
  assert.deepEqual(JSON.parse(request.options.body), original);
  assert.equal(request.options.headers['X-PairRoom-CSRF'], session('user-a', 'same-user-new-session').csrf);
  reloaded.complete(request, 200, { outcome: 'replayed' }); await resolve;
  assert.equal(reloaded.requests.filter((r) => r.url.endsWith('/messages') && r.options.method === 'POST').length, 0);
  assert.equal(storage.length, 0);
});

test('NotCommitted permits only explicit retry with the same operation identity and frozen message', async () => {
  const f = await signIn(), original = await uncertainMessage(f);
  const resolve = f.element('resolve-operation').fire('click'); f.reply('/rooms/room-a/resolve-message', 409, { outcome: 'not_committed' }); await resolve;
  assert.equal(f.element('retry-operation').hidden, false);
  assert.equal(f.requests.filter((r) => r.url.endsWith('/messages')).length, 1);
  f.element('message-text').value = 'edited draft must not change original';
  const retry = f.element('retry-operation').fire('click'); const request = f.take('/rooms/room-a/messages');
  assert.deepEqual(JSON.parse(request.options.body), original); f.complete(request, 503, { outcome: 'unknown' }); await retry;
});

test('journal storage failure is explicit and an uncertain action remains exportable in page memory', async () => {
  const storage = { get length() { throw new Error('storage denied'); }, setItem() { throw new Error('denied'); } };
  const f = await signIn(fixture({ storage })); await uncertainMessage(f);
  assert.match(f.element('journal-status').textContent, /reload recovery is unavailable/);
  assert.equal(f.element('pending').hidden, false);
  assert.equal(f.text().includes('private-csrf'), false);
});

test('journal restore never loads another origin/deployment or an arbitrary mutation URL', async () => {
  const storage = new Storage(), f = await signIn(fixture({ storage })); await uncertainMessage(f);
  const [key, encoded] = [...storage.values.entries()][0]; storage.removeItem(key);
  const original = JSON.parse(encoded);
  for (const [field, value] of [['origin', 'https://other.example'], ['deployment', 'other-deployment'], ['user', 'user-b']]) {
    const record = structuredClone(original); record.scope[field] = value;
    storage.setItem('pairroom.enterprise.pending.v1:' + JSON.stringify([record.scope.origin, record.scope.deployment, record.scope.user, record.scope.room, record.body.client_id]), JSON.stringify(record));
  }
  const malicious = structuredClone(original); malicious.path = 'https://attacker.example/steal';
  storage.setItem(key, JSON.stringify(malicious));
  const reloaded = await signIn(fixture({ storage })); assert.equal(reloaded.element('pending').hidden, true);
  assert.equal(reloaded.requests.filter((r) => r.options.method === 'POST').length, 0);
});

test('My Rooms keyset pagination and Project metadata remain plain text', async () => {
  const f = fixture({ autoCatalog: false }); await signIn(f);
  const first = f.take('/catalog/rooms?limit=25&state=active');
  f.complete(first, 200, { resource: 'rooms', rooms: [{ room_id: 'room-a', name: '<img onerror=steal()>', role: 'member', project_name: 'Logical project' }], next: 'room-a' }); await flush();
  assert.equal(f.element('room-list').text().includes('<img onerror=steal()>'), true); assert.equal(f.element('rooms-more').hidden, false);
  f.element('rooms-more').fire('click'); const next = f.take('/catalog/rooms?limit=25&state=active&after=room-a');
  f.complete(next, 200, { resource: 'rooms', rooms: [{ room_id: 'room-b', name: 'Second', role: 'viewer' }] }); await flush();
  assert.equal(f.element('room-list').children.length, 2); assert.equal(f.element('rooms-more').hidden, true);
});

test('late Room list and Agent responses cannot populate a new account workspace', async () => {
  const f = fixture({ autoCatalog: false }); await signIn(f); const old = f.take('/catalog/rooms?limit=25&state=active');
  const refresh = f.element('refresh').fire('click'); await signIn(f, session('user-b', 'session-b')); await refresh;
  f.complete(old, 200, { resource: 'rooms', rooms: [{ room_id: 'private-old-room', name: 'Old secret', role: 'owner' }] }, session()); await flush();
  assert.equal(f.text().includes('Old secret'), false);
  assert.equal(f.element('room-list').children.length, 0);
});

test('archive sends metadata only and does not invent process-control requests', async () => {
  const f = await signIn(); await open(f);
  const operation = f.element('archive-room').fire('click'), request = f.take('/catalog/commands');
  assert.deepEqual(JSON.parse(request.options.body).intent, { kind: 'room.archive', room_id: 'room-a' });
  assert.equal(f.requests.some((r) => /stop|interrupt|process/.test(r.url)), false);
  f.complete(request, 503, { outcome: 'unknown' }); await operation;
});

test('governance requires selected exact action and never sends a fabricated fresh boolean', async () => {
  const f = await signIn(fixture(), { ...session(), administrator: true });
  f.element('governance-room').value = 'room-private'; f.element('governance-owner').value = 'disabled-owner'; f.element('governance-target').value = 'target'; f.element('governance-revision').value = '7';
  const operation = f.element('governance-form').fire('submit'), request = f.take('/governance/actions');
  const body = JSON.parse(request.options.body); assert.equal('fresh' in body, false); assert.equal('auth_time' in body, false);
  assert.deepEqual(body.intent, { room_id: 'room-private', expected_owner_user_id: 'disabled-owner', target_user_id: 'target', expected_revision: 7 });
  f.complete(request, 200, { action: { id: 'action-1' }, authorization_url: 'https://idp.example.test/authorize?state=browser-bound' }); await operation;
  assert.deepEqual(f.navigations, ['https://idp.example.test/authorize?state=browser-bound']);
  assert.equal(f.requests.some((r) => r.url.endsWith('/ownership')), false);
});

test('only a proven uncommitted operation can be explicitly discarded', async () => {
  const f = await signIn(); await uncertainMessage(f);
  await f.element('discard-operation').fire('click'); assert.equal(f.storage.length, 1);
  const resolve = f.element('resolve-operation').fire('click'); f.reply('/rooms/room-a/resolve-message', 409, { outcome: 'not_committed' }); await resolve;
  assert.equal(f.element('discard-operation').hidden, false);
  await f.element('discard-operation').fire('click');
  assert.equal(f.storage.length, 0); assert.equal(f.element('pending').hidden, true);
});

test('importing a recovery record is scope checked and never performs an action', async () => {
  const original = await signIn(); await uncertainMessage(original);
  const encoded = [...original.storage.values.values()][0];
  const different = await signIn(fixture(), session('user-b', 'session-b'));
  different.element('import-operation').files = [{ size: encoded.length, text: async () => encoded }];
  await different.element('import-operation').fire('change');
  assert.equal(different.element('pending').hidden, true); assert.equal(different.storage.length, 0);
  const same = await signIn(); same.element('import-operation').files = [{ size: encoded.length, text: async () => encoded }];
  await same.element('import-operation').fire('change'); assert.equal(same.element('pending').hidden, false);
  assert.equal(same.requests.filter((request) => request.options.method === 'POST').length, 0);
  assert.equal(same.storage.length, 1);
});

test('recovery imports reject credential-shaped and unknown fields instead of persisting them', async () => {
  const original = await signIn(); await uncertainMessage(original);
  const record = JSON.parse([...original.storage.values.values()][0]); record.body.csrf = 'must-not-persist';
  const encoded = JSON.stringify(record), f = await signIn();
  f.element('import-operation').files = [{ size: encoded.length, text: async () => encoded }];
  await f.element('import-operation').fire('change');
  assert.equal(f.storage.length, 0); assert.equal(f.element('pending').hidden, true);
  assert.equal(f.requests.filter((request) => request.options.method === 'POST').length, 0);
});

test('Owner queue controls keep original mutation identity and never retry uncertain output automatically', async () => {
  for (const [state, kind] of [['queued', 'cancel'], ['unknown', 'retry']]) {
    const f = await signIn(); f.element('room-id').value = 'room-a'; const reading = f.element('room-form').fire('submit'); await flush();
    f.reply('/rooms/room-a', 200, { ...history(), target_generations: { slot1: 1 }, messages: [{ id: 'message-1', from: 'user', to: 'slot1', text: 'Work', state, target_generation: 1 }] }); await reading;
    const item = f.element('messages').children[0], action = item.children.find((value) => value.listeners.has('click'));
    assert.ok(action); assert.equal(action.disabled, false);
    const operation = action.fire('click'), request = f.take('/rooms/room-a/queue'), body = JSON.parse(request.options.body);
    assert.deepEqual(body.intent, { kind, message_id: 'message-1' }); assert.equal(body.expected_revision, 1);
    f.complete(request, 503, { outcome: 'unknown' }); await operation;
    assert.equal(f.requests.filter((value) => value.url.endsWith('/queue')).length, 1);
    const resolve = f.element('resolve-operation').fire('click');
    assert.deepEqual(JSON.parse(f.take('/rooms/room-a/resolve-queue').options.body), body);
    f.reply('/rooms/room-a/resolve-queue', 200, { outcome: 'replayed' }); await resolve;
  }
});

test('real empty snapshot Binding objects render Vacant while historical retirement stays Retired', async () => {
  const f = await signIn(); f.element('room-id').value = 'room-a'; const reading = f.element('room-form').fire('submit'); await flush();
  f.reply('/rooms/room-a', 200, { ...history(), bindings: { slot1: { bind_id: '', generation: 0, active: false }, slot2: { bind_id: 'old-binding', generation: 4, active: false } }, target_generations: { slot1: 1, slot2: 4 } });
  await reading;
  assert.match(f.element('slots').children[0].text(), /Vacant/);
  assert.equal(f.element('slots').children[0].text().includes('Retired'), false);
  assert.match(f.element('slots').children[1].text(), /Retired · generation 4/);
});

test('Room and Project names use UTF-8 bytes and control rejection before any effect request', async () => {
  for (const name of ['验'.repeat(60), 'x'.repeat(161), 'control\u0007name']) {
    const f = await signIn();
    f.element('create-project-name').value = name; await f.element('create-project-form').fire('submit');
    f.element('create-room-name').value = name; await f.element('create-room-form').fire('submit');
    await open(f); f.element('room-name').value = name; await f.element('rename-room-form').fire('submit');
    assert.equal(f.requests.filter((request) => request.options.method === 'POST').length, 0);
    assert.equal(f.storage.length, 0); assert.equal(f.element('pending').hidden, true);
    assert.match(f.element('status').textContent, /1–160 UTF-8 bytes/);
  }
  for (const name of ['x'.repeat(160), '验'.repeat(53)]) {
    const f = await signIn(); f.element('create-project-name').value = name;
    const operation = f.element('create-project-form').fire('submit');
    const request = f.take('/catalog/commands'); assert.equal(JSON.parse(request.options.body).intent.name, name);
    f.complete(request, 503, { outcome: 'unknown' }); await operation;
  }
});

test('first precise typed validation refusal is retained accurately and explicitly discardable after reload', async () => {
  const f = await signIn(); await open(f); f.element('room-project-id').value = 'bad/path';
  const operation = f.element('room-project-form').fire('submit'); f.reply('/catalog/commands', 400, { outcome: 'refused', class: 'invalid_request' }); await operation;
  assert.equal(f.element('pending-title').textContent, 'Action rejected'); assert.equal(f.element('resolve-operation').hidden, true);
  assert.equal(f.element('discard-operation').hidden, false); assert.equal(f.element('retry-operation').hidden, true);
  const record = JSON.parse([...f.storage.values.values()][0]);
  assert.deepEqual(record.validation_refusal, { http_status: 400, outcome: 'refused', class: 'invalid_request' });
  const reloaded = await signIn(fixture({ storage: f.storage }));
  assert.equal(reloaded.element('pending-title').textContent, 'Action rejected');
  assert.equal(reloaded.requests.filter((request) => request.options.method === 'POST').length, 0);
  await reloaded.element('discard-operation').fire('click'); assert.equal(reloaded.storage.length, 0); assert.equal(reloaded.element('pending').hidden, true);
});

test('generic errors, authorization refusals and Resolve refusals never become a definite validation rejection', async () => {
  for (const [status, data] of [[400, { error: 'invalid_request' }], [403, { outcome: 'refused', class: 'authorization' }], [400, { outcome: 'refused', class: 'invalid_request', fact: {} }], [503, { outcome: 'unknown' }]]) {
    const f = await signIn(); await open(f); f.element('room-project-id').value = 'project-1';
    const operation = f.element('room-project-form').fire('submit'); f.reply('/catalog/commands', status, data); await operation;
    assert.equal(f.element('discard-operation').hidden, true); assert.equal(f.element('retry-operation').hidden, true);
    assert.equal(JSON.parse([...f.storage.values.values()][0]).validation_refusal, undefined);
    const check = f.element('resolve-operation').fire('click'); f.reply('/catalog/resolve', 400, { outcome: 'refused', class: 'invalid_request' }); await check;
    assert.equal(f.element('discard-operation').hidden, true);
  }
});

test('an unresolved Room blocks only that Room while another Room can commit without losing evidence', async () => {
  const f = await signIn(); const first = await uncertainMessage(f);
  f.element('message-text').value = 'accidental same-Room repetition'; await f.element('send-form').fire('submit');
  assert.equal(f.requests.filter((request) => request.url.endsWith('/room-a/messages')).length, 1);
  await open(f, 'room-b', 'Room B history'); f.element('message-text').value = 'independent Room B work';
  const second = f.element('send-form').fire('submit'); f.reply('/rooms/room-b/messages', 200, { message: { id: 'message-b' } }); await second;
  assert.equal(f.storage.length, 1); assert.equal(JSON.parse([...f.storage.values.values()][0]).body.client_id, first.client_id);
  assert.match(f.element('pending-description').textContent, /room-a/);
});

test('out-of-order operations on different Rooms settle independently and never retarget the selected Room', async () => {
  const f = await signIn(); await open(f); f.element('message-text').value = 'Room A work';
  const first = f.element('send-form').fire('submit'), requestA = f.take('/rooms/room-a/messages');
  await open(f, 'room-b', 'Room B history'); f.element('message-text').value = 'Room B work';
  const second = f.element('send-form').fire('submit'); f.reply('/rooms/room-b/messages', 503, { outcome: 'unknown' }); await second;
  f.complete(requestA, 200, { message: { id: 'message-a' } }); await first;
  assert.equal(f.element('room-title').textContent, 'room-b'); assert.match(f.element('pending-description').textContent, /room-b/);
  assert.equal(f.storage.length, 1); assert.equal(JSON.parse([...f.storage.values.values()][0]).scope.room, 'room-b');
});

test('NotCommitted from one resource cannot enable retry or discard for another selected journal', async () => {
  const f = await signIn(); await uncertainMessage(f);
  await open(f, 'room-b'); f.element('message-text').value = 'B work'; const second = f.element('send-form').fire('submit'); f.reply('/rooms/room-b/messages', 503, { outcome: 'unknown' }); await second;
  f.element('pending-list').children.find((value) => value.text().includes('room:room-a')).fire('click');
  const resolving = f.element('resolve-operation').fire('click');
  f.element('pending-list').children.find((value) => value.text().includes('room:room-b')).fire('click');
  f.reply('/rooms/room-a/resolve-message', 409, { outcome: 'not_committed' }); await resolving;
  assert.equal(f.element('retry-operation').hidden, true); assert.equal(f.element('discard-operation').hidden, true);
  f.element('pending-list').children.find((value) => value.text().includes('room:room-a')).fire('click');
  assert.equal(f.element('retry-operation').hidden, false); assert.equal(f.element('discard-operation').hidden, false);
});

test('a stale same-user response cannot clear the busy guard for a new session resolving the same operation', async () => {
  const f = await signIn(); await uncertainMessage(f);
  const oldResolve = f.element('resolve-operation').fire('click'), old = f.take('/rooms/room-a/resolve-message');
  const refresh = f.element('refresh').fire('click'); await signIn(f, session('user-a', 'new-session')); await refresh;
  const currentResolve = f.element('resolve-operation').fire('click');
  const currentRequest = f.requests.filter((request) => !request.resolved && request.url.endsWith('/rooms/room-a/resolve-message')).at(-1);
  f.complete(old, 409, { outcome: 'not_committed' }, session()); await oldResolve;
  assert.equal(f.element('resolve-operation').disabled, true);
  f.complete(currentRequest, 409, { outcome: 'not_committed' }); await currentResolve;
  assert.equal(f.element('resolve-operation').disabled, false);
});

test('another resource completing cannot erase an unsaved in-memory unknown when browser storage fails', async () => {
  const storage = { get length() { throw new Error('denied'); }, setItem() { throw new Error('denied'); }, removeItem() { throw new Error('denied'); } };
  const f = await signIn(fixture({ storage })); await uncertainMessage(f);
  await open(f, 'room-b'); f.element('message-text').value = 'B work'; const second = f.element('send-form').fire('submit'); f.reply('/rooms/room-b/messages', 200, { message: { id: 'message-b' } }); await second;
  assert.match(f.element('pending-description').textContent, /room-a/); assert.match(f.element('journal-status').textContent, /reload recovery is unavailable/);
});
