'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { fixture, signIn, session, history, open, flush, Hub, Storage } = require('./enterprise_web_test_helpers');

const admin = () => ({ ...session(), administrator: true });
const action = () => ({ id: 'action-original', intent: { room_id: 'room-private', expected_owner_user_id: 'owner-disabled', target_user_id: 'member', expected_revision: 2, reason: 'private recovery reason' }, ready: true, expires_at: new Date(Date.now() + 60000).toISOString() });
async function prepare(f, prepared = action()) {
  f.element('tab-governance').fire('click');
  f.reply('/governance/actions', 200, { actions: [prepared] }); await flush();
  return f.element('governance-actions').children[0].children.find(e => e.textContent === 'Transfer this Room');
}
async function settle(f, request, outcome = 'committed') {
  f.complete(request, 200, { outcome }); await flush(); await flush();
  // Reproduce the real administrator's correct content denial if the old UI
  // wrongly follows the metadata-only operation with a Room content request.
  for (const read of f.requests.filter(r => !r.resolved && r.url.endsWith('/rooms/room-private'))) f.complete(read, 403, { outcome: 'refused', class: 'authorization' });
  await flush();
}
function confirmedWithoutRoomBody(f, baseline) {
  assert.match(f.element('status').textContent, /^Action confirmed: Transfer Room ownership/);
  const requests = f.requests.slice(baseline);
  assert.equal(requests.some(r => r.url.endsWith('/rooms/room-private')), false, 'governance must not read Room body');
  assert.equal(requests.some(r => r.url.includes('/catalog/room?')), false, 'governance stays in its metadata view');
  assert.equal(f.element('governance-panel').hidden, false);
  const refresh = requests.find(r => r.url.endsWith('/governance/actions') && !r.options.method);
  assert.ok(refresh, 'refresh action metadata after confirmed result');
  f.complete(refresh, 200, { actions: [] });
}

test('confirmed governance retains success feedback without attempting administrator Room content', async () => {
  const f = await signIn(fixture(), admin()), button = await prepare(f), baseline = f.requests.length;
  const transfer = button.click(), request = f.take('/rooms/room-private/ownership');
  assert.equal(JSON.parse(request.options.body).action_id, 'action-original');
  await settle(f, request); await transfer;
  confirmedWithoutRoomBody(f, baseline); await flush();
  assert.match(f.element('status').textContent, /^Action confirmed:/);
  assert.equal(f.requests.slice(baseline).filter(r => r.options.method === 'POST').length, 1);
  assert.equal(f.storage.length, 0);
});

test('explicit resolution of lost governance response keeps the original identity and metadata view', async () => {
  const first = await signIn(fixture(), admin()), button = await prepare(first);
  const transfer = button.click(), original = first.take('/rooms/room-private/ownership'), body = JSON.parse(original.options.body);
  original.reject(new Error('response lost')); await transfer;
  assert.equal(first.storage.length, 1);
  const f = await signIn(fixture({ storage: first.storage }), admin()), baseline = f.requests.length;
  assert.equal(f.requests.filter(r => r.options.method === 'POST').length, 0);
  const resolution = f.element('resolve-operation').fire('click'), request = f.take('/rooms/room-private/resolve-ownership');
  assert.deepEqual(JSON.parse(request.options.body), body);
  await settle(f, request, 'replayed'); await resolution;
  confirmedWithoutRoomBody(f, baseline); await flush();
  assert.equal(f.requests.slice(baseline).filter(r => r.options.method === 'POST').length, 1);
  assert.equal(f.requests.some(r => r.url.endsWith('/ownership') && r.options.method === 'POST'), false);
});

test('ordinary live Owner transfer still refreshes the selected Room', async () => {
  const f = await signIn(); await open(f); f.element('ownership-target').value = 'member';
  const baseline = f.requests.length, transfer = f.element('ownership-form').fire('submit'), request = f.take('/rooms/room-a/ownership');
  assert.equal('action_id' in JSON.parse(request.options.body), false);
  f.complete(request, 200, { outcome: 'committed' }); await transfer; await flush();
  assert.ok(f.requests.slice(baseline).find(r => r.url.endsWith('/rooms/room-a') && !r.options.method));
  assert.equal(f.requests.slice(baseline).some(r => r.url.endsWith('/governance/actions')), false);
});

test('same-scope tab hint preserves an unknown journal until explicit original Resolve', async () => {
  const hub = new Hub(), storage = new Storage(), a = await signIn(fixture({ hub, storage }), admin()), button = await prepare(a);
  const transfer = button.click(), original = a.take('/rooms/room-private/ownership'), body = JSON.parse(original.options.body);
  original.reject(new Error('response lost')); await transfer;
  const b = await signIn(fixture({ hub, storage }), admin()); await flush(); const baseline = b.requests.length;
  const resolveA = a.element('resolve-operation').fire('click'); a.reply('/rooms/room-private/resolve-ownership', 200, { outcome: 'replayed' }); await resolveA; await flush();
  assert.equal(storage.length, 0);
  assert.equal(b.element('pending').hidden, false, 'another tab hint is not service confirmation');
  assert.equal(b.element('discard-operation').hidden, true);
  assert.equal(b.element('retry-operation').hidden, true);
  assert.match(b.element('status').textContent, /another tab.*Refresh.*Resolve/);
  assert.equal(b.requests.length, baseline, 'hint must not read or send anything');
  const changes = hub.sent.filter(d => d.origin);
  assert.equal(changes.length, 1);
  assert.deepEqual(Object.keys(changes[0]).sort(), ['deployment', 'kind', 'origin', 'user']);
  assert.equal(JSON.stringify(changes).includes('private'), false);
  assert.equal(JSON.stringify(changes).includes(body.client_id), false);
  const resolveB = b.element('resolve-operation').fire('click'), request = b.take('/rooms/room-private/resolve-ownership');
  assert.deepEqual(JSON.parse(request.options.body), body);
  assert.equal(b.requests.slice(baseline).filter(r => r.options.method === 'POST').length, 1);
  b.complete(request, 200, { outcome: 'replayed' }); await resolveB; await flush();
  assert.equal(b.element('pending').hidden, true);
});

test('recovery hints for other scopes cannot clear journals or trigger requests', async () => {
  const f = await signIn(fixture(), admin()), button = await prepare(f);
  const transfer = button.click(), original = f.take('/rooms/room-private/ownership'); original.reject(new Error('response lost')); await transfer;
  const receiver = f.hub.channels.find(c => c.name === 'pairroom.enterprise.recovery.v1');
  assert.ok(receiver);
  const baseline = f.requests.length, text = f.element('status').textContent;
  for (const data of [{ kind: 'recovery_changed', origin: 'https://foreign.example', deployment: 'deployment-1', user: 'user-a' }, { kind: 'recovery_changed', origin: 'https://service.example.test', deployment: 'deployment-other', user: 'user-a' }, { kind: 'recovery_changed', origin: 'https://service.example.test', deployment: 'deployment-1', user: 'user-b' }, { kind: 'recovery_changed', origin: 'https://service.example.test', deployment: 'deployment-1' }, { kind: 'wrong_kind', origin: 'https://service.example.test', deployment: 'deployment-1', user: 'user-a' }, { kind: 'recovery_changed', origin: 'https://service.example.test', deployment: 'deployment-1', user: 'user-a', body: 'untrusted body' }]) receiver.onmessage({ data });
  await flush(); assert.equal(f.requests.length, baseline); assert.equal(f.element('status').textContent, text);
  assert.equal(f.element('pending').hidden, false); assert.equal(f.storage.length, 1);
});


test('malformed action identity cannot submit or create a governance recovery journal', async () => {
  for (const id of [{ unexpected: true }, 17, 'bad/path']) {
    const f = await signIn(fixture(), admin()), button = await prepare(f, { ...action(), id }), baseline = f.requests.length;
    await button.click(); await flush();
    assert.equal(f.requests.length, baseline);
    assert.equal(f.storage.length, 0);
    assert.equal(f.element('pending').hidden, true);
  }
});

test('failed optional recovery notification cannot relabel confirmed governance or stop metadata refresh', async () => {
  const f = await signIn(fixture(), admin()), button = await prepare(f), baseline = f.requests.length;
  f.hub.channels.find(c => c.name === 'pairroom.enterprise.recovery.v1').postMessage = () => { throw new DOMException('channel unavailable', 'InvalidStateError'); };
  const transfer = button.click(), request = f.take('/rooms/room-private/ownership');
  await settle(f, request); await transfer;
  confirmedWithoutRoomBody(f, baseline); await flush();
  assert.equal(f.storage.length, 0);
  assert.equal(f.element('pending').hidden, true);
});

test('failed optional recovery notification leaves ordinary Owner success and Room refresh intact', async () => {
  const f = await signIn(); await open(f); f.element('ownership-target').value = 'member'; const baseline = f.requests.length;
  f.hub.channels.find(c => c.name === 'pairroom.enterprise.recovery.v1').postMessage = () => { throw new DOMException('channel unavailable', 'InvalidStateError'); };
  const transfer = f.element('ownership-form').fire('submit'), request = f.take('/rooms/room-a/ownership');
  f.complete(request, 200, { outcome: 'committed' }); await transfer; await flush();
  assert.doesNotMatch(f.element('status').textContent, /uncertain/);
  const refreshing = f.requests.slice(baseline).find(r => r.url.endsWith('/rooms/room-a') && !r.options.method);
  assert.ok(refreshing);
  f.complete(refreshing, 200, history('room-a', 'owner-refreshed-message')); await flush();
  assert.equal(f.text().includes('owner-refreshed-message'), true);
  assert.equal(f.element('pending').hidden, true);
  assert.equal(f.storage.length, 0);
});

test('failed optional recovery channel construction preserves session initialization and governance', async () => {
  class RecoveryUnavailableHub extends Hub {
    implementation() {
      const Base = super.implementation();
      return class extends Base {
        constructor(name) {
          if (name === 'pairroom.enterprise.recovery.v1') throw new DOMException('channel disabled', 'SecurityError');
          super(name);
        }
      };
    }
  }
  const hub = new RecoveryUnavailableHub(), f = await signIn(fixture({ hub }), admin());
  assert.ok(hub.channels.find(c => c.name === 'pairroom.enterprise.account.v1'));
  assert.equal(f.element('workspace').hidden, false);
  const button = await prepare(f), baseline = f.requests.length, transfer = button.click(), request = f.take('/rooms/room-private/ownership');
  await settle(f, request); await transfer;
  confirmedWithoutRoomBody(f, baseline); await flush();
  assert.equal(f.storage.length, 0);
});
