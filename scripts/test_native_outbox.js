'use strict';
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const {locks} = require('./native_browser_test_helpers');
function boot(manager = locks()) {
  const window = {};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/native-outbox.js','utf8'), {window, TextEncoder, navigator: {locks: manager}});
  return window.PairRoomNativeOutbox;
}
async function main() {
  const data = new Map();
  const storage = {getItem: k => data.get(k) ?? null, setItem: (k,v) => data.set(k,v), removeItem: k => data.delete(k)};
  const payload = {id:'original-id', to:'slot1', text:'Plan **A**', attachment_ids:['image-id']};
  const manager = locks();
  let box = boot(manager);
  await box.save(storage,'room-id',payload);
  box = boot(manager); // New context, same persistent recovery record.
  assert.equal(JSON.stringify(box.load(storage,'room-id')),JSON.stringify(payload));
  assert.equal(box.load(storage,'another-room'),null);
  await assert.rejects(async()=>box.save(storage,'room-id',{...payload,id:'new-id'}),/outbox_conflict/);
  await assert.rejects(async()=>box.clear(storage,'room-id','new-id'),/outbox_conflict/);
  await assert.rejects(async()=>box.save(storage,'empty-room',{...payload,token:'secret'}),/outbox_invalid/);
  await assert.rejects(async()=>box.save({...storage,setItem(){throw Error('quota');}},'empty-room',payload),/quota/);
  assert(box.matches(payload,{from:'user',to:'slot1',text:payload.text,attachments:[{id:'image-id'}]}));
  assert(!box.matches(payload,{from:'slot1',to:'slot1',text:payload.text,attachments:[{id:'image-id'}]}));

  // A second tab holding this Room's lock must prevent *all* mutations, without
  // delaying an explicit send in a hidden-tab lock queue or touching other Rooms.
  await manager.request('pairroom.native.outbox.v1.room-id', {mode:'exclusive',ifAvailable:true}, async()=>{
    // Busy is transient and distinct from a mismatched record (outbox_conflict).
    await assert.rejects(async()=>box.save(storage,'room-id',payload),/outbox_busy/);
    await assert.rejects(async()=>box.clear(storage,'room-id',payload.id),/outbox_busy/);
    await assert.rejects(async()=>box.forget(storage,'room-id',storage.getItem('pairroom.native.outbox.v1.room-id')),/outbox_busy/);
    await box.save(storage,'another-room',payload);
    assert.equal(box.load(storage,'room-id').id,payload.id);
  });
  const second = boot(manager);
  await box.clear(storage,'room-id',payload.id);
  const outcomes = await Promise.allSettled([
    box.save(storage,'room-id',payload),
    second.save(storage,'room-id',{...payload,id:'other-id'})
  ]);
  assert.equal(outcomes.filter(o=>o.status==='fulfilled').length,1);
  assert.equal(box.load(storage,'room-id').id,payload.id);
  const observed = box.capture(storage,'room-id');
  await box.clear(storage,'room-id',payload.id);
  await second.save(storage,'room-id',{...payload,id:'newer-id'});
  await assert.rejects(()=>box.forget(storage,'room-id',observed),/outbox_conflict/);
  await assert.rejects(()=>box.forget(storage,'room-id'),/outbox_conflict/);
  assert.equal(box.load(storage,'room-id').id,'newer-id','stale confirmation deleted newer work');
  await assert.rejects(()=>boot(null).save(storage,'unsupported-room',payload),/outbox_unavailable/);
  assert.equal(box.load(storage,'unsupported-room'),null,'missing locks fell back to unsafe publication');

  // Explicit Forget can repair corruption, but only the bytes actually confirmed.
  storage.setItem('pairroom.native.outbox.v1.room-id','not-json');
  assert.throws(()=>box.load(storage,'room-id'),/outbox_invalid/);
  await box.forget(storage,'room-id',box.capture(storage,'room-id'));
  assert.equal(box.load(storage,'room-id'),null);
  console.log('Native outbox: refresh recovery, cross-tab exclusion, stale Forget, unavailable locks and receipt matching: ok');
}
main().catch(error=>{console.error(error);process.exitCode=1;});
