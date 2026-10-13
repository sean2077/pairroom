'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { fixture, signIn, session, flush } = require('./enterprise_web_test_helpers');
const admin = (id = 'session-a') => ({ ...session('user-a', id), administrator: true });
async function load(f, resource, data) { f.reply('/administration/' + resource + '?limit=25', 200, { resource, ...data }); await flush(); }

test('administration is hidden from ordinary Users and clears private directory and exact subject on account change', async () => {
  const plain = await signIn(); assert.equal(plain.element('tab-administration').hidden, true);
  const f = await signIn(fixture(), admin()); f.element('tab-administration').fire('click');
  await load(f, 'users', { users: [{ user_id: 'private-user', disabled: false }] });
  f.element('admission-subject').value = 'private exact subject ';
  const refreshing = f.element('refresh').fire('click'); f.reply('/session', 200, session('user-b', 'session-b'), session('user-b', 'session-b')); await refreshing;
  assert.equal(f.element('tab-administration').hidden, true); assert.equal(f.element('admission-subject').value, ''); assert.equal(f.text().includes('private-user'), false);
  await load(f, 'admissions', { admissions: [{ subject: 'late-private-subject' }] }); assert.equal(f.text().includes('late-private-subject'), false);
});

test('administrator directories paginate and permit disabled role removal while disabled grants stay unavailable', async () => {
  const f = await signIn(fixture(), admin()); f.element('tab-administration').fire('click');
  await load(f, 'users', { users: [{ user_id: 'disabled-admin', disabled: true, administrator: true }, { user_id: 'disabled-user', disabled: true, administrator: false }], next: 'cursor-opaque' });
  const [a, u] = f.element('administration-users').children;
  assert.equal(a.children.find(x => x.text() === 'Remove administrator role').disabled, false);
  assert.equal(u.children.find(x => x.text() === 'Grant administrator role').disabled, true);
  a.children.find(x => x.text() === 'Remove administrator role').fire('click'); await flush();
  const request = f.take('/administration/commands'), body = JSON.parse(request.options.body);
  assert.deepEqual(body.intent, { kind: 'administrator.set', target_user_id: 'disabled-admin', enabled: false });
  f.element('administration-users-more').fire('click'); assert.ok(f.requests.some(x => x.url.endsWith('/administration/users?limit=25&after=cursor-opaque')));
});

test('exact identity admission keeps opaque whitespace and unknown journal survives reload with explicit original Resolve', async () => {
  const f = await signIn(fixture(), admin());
  f.element('admission-connection').value = 'corporate'; f.element('admission-issuer').value = 'https://idp.example.test/issuer'; f.element('admission-subject').value = ' Case-sensitive subject '; f.element('admission-enabled').value = 'true';
  f.element('admission-form').fire('submit'); await flush(); const submit = f.take('/administration/commands'), original = JSON.parse(submit.options.body);
  assert.equal(original.intent.identity.subject, ' Case-sensitive subject '); submit.reject(new Error('response lost')); await flush();
  const restored = await signIn(fixture({ storage: f.storage }), admin('session-new'));
  assert.equal(restored.requests.some(x => x.options.method === 'POST'), false); assert.equal(restored.element('retry-operation').hidden, true);
  restored.element('resolve-operation').fire('click'); await flush(); const resolve = restored.take('/administration/resolve');
  assert.deepEqual(JSON.parse(resolve.options.body), original); restored.complete(resolve, 200, { outcome: 'replayed' }); await flush(); assert.equal(restored.storage.length, 0);
});

test('an unknown User administration action blocks only that User and preserves original key', async () => {
  const f = await signIn(fixture(), admin()); f.element('tab-administration').fire('click');
  await load(f, 'users', { users: [{ user_id: 'target-a', disabled: false }, { user_id: 'target-b', disabled: false }] });
  const [a, b] = f.element('administration-users').children;
  const first = a.children.find(x => x.text() === 'Disable User'); first.fire('click'); await flush(); f.take('/administration/commands').reject(new Error('unknown')); await flush();
  a.children.find(x => x.text() === 'Grant administrator role').fire('click'); await flush(); assert.equal(f.requests.filter(x => x.url.endsWith('/administration/commands')).length, 1);
  b.children.find(x => x.text() === 'Disable User').fire('click'); await flush(); assert.equal(f.requests.filter(x => x.url.endsWith('/administration/commands')).length, 2); assert.equal(f.storage.length, 2);
  assert.ok([...f.storage.values.values()].every(x => !x.includes('private-csrf')));
});
