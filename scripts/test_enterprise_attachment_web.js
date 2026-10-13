'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { fixture, signIn, open, session, history, flush, deferred } = require('./enterprise_web_test_helpers');
const object = 'att-0123456789abcdef01234567';
const file = (text = 'original text evidence') => ({ name: 'evidence.txt', size: Buffer.byteLength(text), arrayBuffer: async () => Uint8Array.from(Buffer.from(text)).buffer });
const drafts = f => [...f.storage.values.values()].map(x => JSON.parse(x)).filter(x => x.spec);
const action = (f, name) => { const value = f.element('attachment-drafts').children.flatMap(x => x.children).find(x => x.text() === name); assert.ok(value, name); return value; };
async function prepare(f, bytes = file()) { f.element('attachment-file').files = [bytes]; await f.element('attachment-form').fire('submit'); await flush(); return drafts(f)[0]; }
const receipt = (draft, kind) => ({ version: 1, kind, room_id: draft.scope.room, object_id: object, state: { reserve: 'reserved', verify: 'verified', cancel: 'abandoned' }[kind], metadata: { id: object, ...draft.spec } });
async function reserve(f, draft) { action(f, 'Reserve upload').fire('click'); await flush(); f.reply('/rooms/room-a/attachments', 200, { outcome: 'committed', receipt: receipt(draft, 'reserve') }); await flush(); }

test('attachment stages are explicit and metadata-only; message recovery freezes exact verified IDs', async () => {
  const f = await open(await signIn()); const draft = await prepare(f);
  assert.equal(f.requests.some(x => x.options.method === 'POST'), false); assert.equal(drafts(f).length, 1); assert.equal(JSON.stringify(drafts(f)).includes('original text evidence'), false);
  await reserve(f, draft); assert.equal(f.requests.filter(x => x.options.method === 'PUT').length, 0);
  action(f, 'Upload selected bytes').fire('click'); await flush(); const upload = f.take('/rooms/room-a/attachments/' + object + '/content');
  assert.equal(upload.options.method, 'PUT'); assert.equal(upload.options.headers['Content-Type'], 'application/octet-stream'); assert.equal(upload.options.body.name, 'evidence.txt');
  f.complete(upload, 200, { status: 'uploaded', object_id: object, size: draft.spec.size, sha256: draft.spec.sha256 }); await flush();
  action(f, 'Verify uploaded attachment').fire('click'); await flush(); const verify = f.take('/rooms/room-a/attachments/' + object + '/verify'); assert.equal(JSON.parse(verify.options.body).client_id, draft.verify_id);
  f.complete(verify, 200, { outcome: 'committed', receipt: receipt(draft, 'verify') }); await flush();
  action(f, 'Include in next message').fire('click'); f.element('message-text').value = '';
  f.element('send-form').fire('submit'); await flush(); const send = f.take('/rooms/room-a/messages'); const intent = JSON.parse(send.options.body);
  assert.deepEqual(intent.attachment_ids, [object]); assert.equal(intent.text, ''); assert.equal(action(f, 'Abandon attachment draft').disabled, true);
  send.reject(new Error('send lost')); await flush(); const reloaded = await signIn(fixture({ storage: f.storage }));
  assert.equal(reloaded.requests.some(x => x.options.method), false); reloaded.element('resolve-operation').fire('click'); await flush(); const resolve = reloaded.take('/rooms/room-a/resolve-message'); assert.deepEqual(JSON.parse(resolve.options.body), intent);
  reloaded.complete(resolve, 200, { outcome: 'replayed' }); await flush(); assert.equal(f.storage.length, 0);
});

test('lost raw upload reload requires exact hash reselect and never uploads automatically', async () => {
  const f = await open(await signIn()), draft = await prepare(f); await reserve(f, draft);
  action(f, 'Upload selected bytes').fire('click'); await flush(); f.take('/rooms/room-a/attachments/' + object + '/content').reject(new Error('upload lost')); await flush();
  const restored = await open(await signIn(fixture({ storage: f.storage })));
  assert.equal(action(restored, 'Retry upload with original bytes').disabled, true); assert.equal(restored.requests.some(x => x.options.method), false);
  const input = restored.element('attachment-drafts').children[0].children.find(x => x.type === 'file'); input.files = [file('changed bytes payload')]; await input.fire('change');
  assert.equal(action(restored, 'Retry upload with original bytes').disabled, true);
  input.files = [file()]; await input.fire('change'); assert.equal(action(restored, 'Retry upload with original bytes').disabled, false);
  assert.equal(restored.requests.some(x => x.options.method), false); action(restored, 'Retry upload with original bytes').fire('click'); await flush(); assert.equal(restored.take('/rooms/room-a/attachments/' + object + '/content').options.method, 'PUT');
});

test('lost reservation and verification retain original IDs across same User new session with explicit read-only Resolve', async () => {
  const f = await open(await signIn()), draft = await prepare(f); action(f, 'Reserve upload').fire('click'); await flush(); f.take('/rooms/room-a/attachments').reject(new Error('lost reservation')); await flush();
  const restored = await open(await signIn(fixture({ storage: f.storage }), session('user-a', 'new-session'))); action(restored, 'Check original reservation').fire('click'); await flush(); const resolve = restored.take('/rooms/room-a/attachments/resolve-reservation');
  assert.equal(JSON.parse(resolve.options.body).client_id, draft.reserve_id); restored.complete(resolve, 200, { outcome: 'replayed', receipt: receipt(draft, 'reserve') }); await flush(); assert.equal(action(restored, 'Upload selected bytes').disabled, true);
  const saved = drafts(restored)[0]; saved.state = 'verifying'; const key = [...f.storage.values.keys()].find(x => x.includes('attachment.v1')); f.storage.setItem(key, JSON.stringify(saved));
  const verification = await open(await signIn(fixture({ storage: f.storage }))); action(verification, 'Check original verification').fire('click'); await flush(); const inspect = verification.take('/rooms/room-a/attachments/' + object + '/resolve-verification'); assert.equal(JSON.parse(inspect.options.body).client_id, draft.verify_id);
  verification.complete(inspect, 409, { outcome: 'not_committed' }); await flush(); assert.ok(action(verification, 'Verify uploaded attachment')); assert.equal(drafts(verification)[0].verify_id, draft.verify_id);
});

test('draft restore rejects credentials and other User/deployment; stale upload cannot restore an old account', async () => {
  const f = await open(await signIn()), draft = await prepare(f); await reserve(f, draft); action(f, 'Upload selected bytes').fire('click'); await flush(); const old = f.take('/rooms/room-a/attachments/' + object + '/content');
  const refresh = f.element('refresh').fire('click'); const identity = session('user-b', 'session-b'); f.reply('/session', 200, identity, identity); await refresh;
  f.complete(old, 200, { status: 'uploaded', object_id: object, size: draft.spec.size, sha256: draft.spec.sha256 }); await flush(); assert.equal(f.element('attachment-drafts').children.length, 0);
  const other = await open(await signIn(fixture({ storage: f.storage }), identity)); assert.equal(other.element('attachment-drafts').children.length, 0);
  const key = [...f.storage.values.keys()][0], bad = JSON.parse(f.storage.getItem(key)); bad.token = 'secret'; f.storage.setItem(key, JSON.stringify(bad)); const malformed = await open(await signIn(fixture({ storage: f.storage }))); assert.equal(malformed.element('attachment-drafts').children.length, 0);
});

test('human history uses stable author provenance and authorized attachment links without relabeling an unknown author Owner', async () => {
  const f = await signIn(); f.element('room-id').value='room-a'; const opening=f.element('room-form').fire('submit');await flush();const value=history();value.total_messages=2;value.messages=[{id:'message-1',from:'user',to:'slot1',state:'queued',text:'first',author:'enterprise:user-original',attachments:[{id:object,name:'<script>.txt',size:20}]},{id:'message-2',from:'user',to:'slot2',state:'queued',text:'old legacy'}];f.reply('/rooms/room-a',200,value);await opening;
  assert.match(f.element('messages').text(),/User user-original/);assert.equal(f.element('messages').text().includes('Owner'),false);const link=f.element('messages').children[0].children.flatMap(child=>child.children).find(child=>child.href);assert.equal(link.href,'/enterprise/api/v1/rooms/room-a/attachments/'+object+'/content');assert.equal(link.textContent,'<script>.txt · 20 bytes');
});

test('latest file reselect wins and a wrong replacement clears previously matching bytes', async () => {
  const f = await open(await signIn()), draft = await prepare(f); await reserve(f, draft);
  const input = f.element('attachment-drafts').children[0].children.find(x => x.type === 'file'), delayed = deferred();
  input.files = [{ ...file(), arrayBuffer: () => delayed.promise }]; const first = input.fire('change');
  input.files = [file('different same length')]; await input.fire('change'); delayed.resolve(await file().arrayBuffer()); await first;
  assert.equal(action(f, 'Upload selected bytes').disabled, true);
  input.files = [file()]; await input.fire('change'); assert.equal(action(f, 'Upload selected bytes').disabled, false);
  input.files = [file('different same length')]; await input.fire('change'); assert.equal(action(f, 'Upload selected bytes').disabled, true);
});

test('metadata import is account scoped and never reserves/uploads/verifies automatically', async () => {
  const f = await open(await signIn()), draft = await prepare(f), encoded = JSON.stringify(draft);
  const restored = await open(await signIn()); restored.element('import-operation').files = [{ size: encoded.length, text: async () => encoded }]; await restored.element('import-operation').fire('change');
  assert.equal(drafts(restored).length, 1); assert.equal(restored.requests.some(x => x.options.method), false); assert.ok(action(restored, 'Reserve upload'));
  const other = await signIn(fixture(), session('user-b','session-b')); other.element('import-operation').files = [{size:encoded.length,text:async()=>encoded}]; await other.element('import-operation').fire('change'); assert.equal(other.storage.length,0);
});

test('cancellation NotCommitted never upgrades an unverified object to verified', async () => {
  const f=await open(await signIn()),draft=await prepare(f);await reserve(f,draft);action(f,'Abandon attachment draft').fire('click');await flush();f.take('/rooms/room-a/attachments/'+object+'/cancel').reject(new Error('cancel lost'));await flush();action(f,'Check original cancellation').fire('click');await flush();f.reply('/rooms/room-a/attachments/'+object+'/resolve-cancellation',409,{outcome:'not_committed'});await flush();
  assert.equal(drafts(f)[0].state,'cancel_ready');assert.equal(f.element('attachment-drafts').text().includes('Include in next message'),false);action(f,'Abandon attachment draft').fire('click');await flush();assert.equal(JSON.parse(f.take('/rooms/room-a/attachments/'+object+'/cancel').options.body).client_id,draft.cancel_id);
});

test('5 MiB file bound, eight Room drafts and 20 MiB message total refuse before network effects', async () => {
  const f=await open(await signIn());f.element('attachment-file').files=[{name:'too-large.txt',size:5*1024*1024+1,arrayBuffer:()=>{throw new Error('must not read oversized bytes')}}];await f.element('attachment-form').fire('submit');assert.equal(f.storage.length,0);assert.equal(f.requests.some(r=>r.options.method),false);
  const template=await prepare(f);f.storage.values.clear();
  for(let i=0;i<8;i++){const draft={...template,id:'draft-'+i,object_id:'att-'+i.toString(16).padStart(24,'0'),state:'verified',spec:{...template.spec,size:3*1024*1024,name:'evidence-'+i+'.txt'}};const key='pairroom.enterprise.attachment.v1:'+JSON.stringify([draft.scope.origin,draft.scope.deployment,draft.scope.user,draft.scope.room,draft.id]);f.storage.setItem(key,JSON.stringify(draft));}
  const restored=await open(await signIn(fixture({storage:f.storage})));for(let i=0;i<8;i++)action(restored,'Include in next message').fire('click');restored.element('message-text').value='too much evidence';restored.element('send-form').fire('submit');await flush();assert.equal(restored.requests.some(r=>r.options.method),false);assert.match(restored.element('status').textContent,/20 MiB/);
  restored.element('attachment-file').files=[file()];await restored.element('attachment-form').fire('submit');assert.equal(drafts(restored).length,8);assert.equal(restored.requests.some(r=>r.options.method),false);
});
