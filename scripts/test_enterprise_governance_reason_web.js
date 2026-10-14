'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { fixture, signIn, session, flush } = require('./enterprise_web_test_helpers');

test('governance refuses invalid private reason before starting sign-in', async () => {
  const f = await signIn(fixture(), { ...session(), administrator: true });
  f.element('governance-room').value = 'room-1'; f.element('governance-owner').value = 'owner'; f.element('governance-target').value = 'member'; f.element('governance-revision').value = '2';
  for (const reason of ['', ' \t\n\u00a0\u3000', '界'.repeat(342), 'reason\u0000', 'reason\u001b', '\ud800']) {
    f.element('governance-reason').value = reason;
    f.element('governance-form').fire('submit'); await flush();
    assert.equal(f.requests.some(r => r.url.endsWith('/governance/actions')), false);
  }
});

test('governance preserves exact reason through preparation and clears it on account change', async () => {
  const f = await signIn(fixture(), { ...session(), administrator: true });
  f.element('governance-room').value = 'room-1'; f.element('governance-owner').value = 'owner'; f.element('governance-target').value = 'member'; f.element('governance-revision').value = '2';
  const reason = ' original\n说明\t '; f.element('governance-reason').value = reason;
  const preparing = f.element('governance-form').fire('submit'); const request = f.take('/governance/actions');
  assert.equal(JSON.parse(request.options.body).intent.reason, reason);
  f.complete(request, 200, { authorization_url: 'https://idp.example.test/reauth' }); await preparing;
  const refresh = f.element('refresh').fire('click'); await signIn(f, session('user-b', 'session-b')); await refresh;
  assert.equal(f.element('governance-reason').value, ''); assert.equal(f.text().includes(reason), false);
});

test('governance counts UTF-8 bytes and the exact Unicode White_Space set', async () => {
  const f = await signIn(fixture(), { ...session(), administrator: true });
  f.element('governance-room').value = 'room-1'; f.element('governance-owner').value = 'owner'; f.element('governance-target').value = 'member'; f.element('governance-revision').value = '2';
  for (const reason of ['界'.repeat(341) + 'a', '\ufeff']) {
    f.element('governance-reason').value = reason;
    const preparing = f.element('governance-form').fire('submit'), request = f.take('/governance/actions');
    assert.equal(JSON.parse(request.options.body).intent.reason, reason);
    f.complete(request, 200, { authorization_url: 'https://idp.example.test/reauth' }); await preparing;
  }
});

test('unknown governance transfer reload resolves the exact original reason and action without resending', async () => {
  const admin = { ...session(), administrator: true }, f = await signIn(fixture(), admin);
  f.element('tab-governance').fire('click');
  const intent = { room_id: 'room-private', expected_owner_user_id: 'owner', target_user_id: 'member', expected_revision: 2, reason: ' original private\n恢复\t ' };
  f.reply('/governance/actions', 200, { actions: [{ id: 'action-original', intent, ready: true, expires_at: new Date(Date.now() + 60000).toISOString() }] }); await flush();
  const card = f.element('governance-actions').children[0];
  assert.equal(card.children[0].textContent, intent.reason);
  const transfer = card.children.find(e => e.textContent === 'Transfer this Room').click();
  const original = f.take('/rooms/room-private/ownership'), body = JSON.parse(original.options.body);
  assert.deepEqual(body.intent, intent); assert.equal(body.action_id, 'action-original');
  original.reject(new Error('response lost')); await transfer;
  const reload = await signIn(fixture({ storage: f.storage }), admin);
  assert.equal(reload.requests.some(r => r.url.endsWith('/ownership')), false);
  const resolve = reload.element('resolve-operation').fire('click'), request = reload.take('/rooms/room-private/resolve-ownership');
  assert.deepEqual(JSON.parse(request.options.body), body);
  reload.complete(request, 200, { outcome: 'replayed' }); await resolve;
  assert.equal(reload.requests.filter(r => r.options.method === 'POST').length, 1);
  assert.equal(reload.storage.length, 0);
});
