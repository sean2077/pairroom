'use strict';
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
function boot() {
  const window = {};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/native-outbox.js','utf8'), {window, TextEncoder});
  return window.PairRoomNativeOutbox;
}
const data = new Map();
const storage = {getItem: k => data.get(k) ?? null, setItem: (k,v) => data.set(k,v), removeItem: k => data.delete(k)};
const payload = {id:'original-id', to:'slot1', text:'Plan **A**', attachment_ids:['image-id']};
let box = boot();
box.save(storage,'room-id',payload);
box = boot(); // Simulated WebView destruction: no state from the previous JS context.
assert.equal(JSON.stringify(box.load(storage,'room-id')),JSON.stringify(payload));
assert.equal(box.load(storage,'another-room'),null);
assert.throws(()=>box.save(storage,'room-id',{...payload,id:'new-id'}),/outbox_conflict/);
assert.throws(()=>box.clear(storage,'room-id','new-id'),/outbox_conflict/);
assert.throws(()=>box.save(storage,'empty-room',{...payload,token:'secret'}),/outbox_invalid/);
assert.throws(()=>box.save({...storage,setItem(){throw Error('quota');}},'empty-room',payload),/quota/);
assert(box.matches(payload,{from:'user',to:'slot1',text:payload.text,attachments:[{id:'image-id'}]}));
assert(!box.matches(payload,{from:'slot1',to:'slot1',text:payload.text,attachments:[{id:'image-id'}]}));
box.clear(storage,'room-id',payload.id);assert.equal(box.load(storage,'room-id'),null);
storage.setItem('pairroom.native.outbox.v1.room-id','not-json');
assert.throws(()=>box.load(storage,'room-id'),/outbox_invalid/);
box.forget(storage,'room-id');
console.log('Native outbox: immutable refresh recovery, storage failure, room isolation and receipt matching: ok');
