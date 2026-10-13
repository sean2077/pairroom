'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { fixture, signIn, session, history, flush } = require('./enterprise_web_test_helpers');

async function openRole(f, id, role) {
  f.element('room-id').value = id; const loading = f.element('room-form').fire('submit'); await flush();
  f.reply('/catalog/room?room_id=' + id, 200, { resource: 'room', rooms: [{ room_id: id, name: id, role, owner_user_id: role === 'owner' ? 'user-a' : 'other', revision: 1 }] }); await flush();
  f.reply('/rooms/' + id, 200, history(id)); await loading;
}

for (const role of ['owner','member','viewer']) test('Room disclosure reflects current ' + role + ' access and clears on account invalidation', async () => {
  const f = await signIn(fixture({ autoCatalog: false })); await openRole(f,'room-a',role);
  assert.ok(f.element('room-access-role').textContent.toLowerCase().startsWith(role + ':'));
  if(role==='viewer') assert.match(f.element('room-access-role').textContent,/new messages|read-only/i);
  const refresh = f.element('refresh').fire('click'); assert.equal(f.element('room-access-role').textContent,'');
  await signIn(f,session('user-b','session-b')); await refresh; assert.equal(f.element('room-access-role').textContent,'');
});

test('stale Room metadata cannot restore an old role disclosure', async () => {
  const f=await signIn(fixture({autoCatalog:false})); await openRole(f,'room-a','owner');
  f.element('room-id').value='room-old';const older=f.element('room-form').fire('submit');await flush();const old=f.take('/catalog/room?room_id=room-old');
  assert.equal(f.element('room-access-role').textContent,'');
  await openRole(f,'room-new','viewer');
  f.complete(old,200,{resource:'room',rooms:[{room_id:'room-old',name:'old',role:'owner',owner_user_id:'user-a',revision:1}]});await older;
  assert.match(f.element('room-access-role').textContent,/^Viewer:/);assert.equal(f.requests.some(r=>r.url.endsWith('/rooms/room-old')),false);
});
