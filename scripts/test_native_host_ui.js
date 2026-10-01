'use strict';
// Execute the shipped client, not a second implementation of its state machine.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');
const {locks, timers} = require('./native_browser_test_helpers');

class Element {
  constructor(tagName = "div") {
    this.tagName = tagName;
    this.children = []; this.events = {}; this.dataset = {}; this.className = '';
    this.value = ''; this.textContent = ''; this.files = []; this.options = [];
    this.disabled = false; this.scrollHeight = 10; this.scrollTop = 0; this.clientHeight = 10;
    this.classList = { toggle() {} };
  }
  appendChild(node) { this.append(node); return node; }
  // The shipped client measures scroll geometry on every render; this flat stub
  // keeps the transcript permanently at its end, as these assertions assume.
  getBoundingClientRect() { return { top: 0, right: 0, bottom: 0, left: 0, width: 0, height: 0 }; }
  get firstElementChild() { return this.children[0] || null; }
  append(...nodes) { for (const node of nodes) { node.parent = this; this.children.push(node); } }
  prepend(node) { node.parent = this; this.children.unshift(node); }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter(n => n !== this); }
  querySelectorAll(selector) { return this.children.filter(n => n.className.split(' ').includes(selector.slice(1))); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  addEventListener(name, fn) { this.events[name] = fn; }
  setAttribute() {}
  contains(node) { return node === this || this.children.some(child => child.contains(node)); }
  focus() {}
}
const tick = () => new Promise(resolve => setImmediate(resolve));
const response = (data, status = 200) => ({ ok: status < 400, status, json: async () => data });
function deferred() { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; }

// The client reports caught failures (including startup exceptions) only in
// #status, so a failed assertion alone can read `'' !== '5000'`. Record each
// page's status history and print the newest pages (the failing step's, including
// concurrent tabs) when the run fails.
const pages = [];
function watchStatus(element, page) {
  let text = '';
  Object.defineProperty(element, 'textContent', { get: () => text, set: value => { text = value; } });
  element.classList = { toggle(name, on) { if (name === 'error' && on) page.errors.push(text); } };
}
function reportPages(limit = 3) {
  const first = Math.max(0, pages.length - limit);
  if (first) console.error(`(${first} earlier Native page(s) omitted)`);
  for (const [index, page] of pages.entries()) {
    if (index < first) continue;
    console.error(`Native page #${index + 1} created at ${page.origin}: status ${JSON.stringify(page.$('status').textContent)}; error statuses ${JSON.stringify(page.errors)}`);
  }
}

async function fixture({ uploadFailure = false, storage, receipts = new Map(), draft = true, manager = locks(), hang = '', receiptGate, transport, sessionFailures = 0, snapshotFailures = 0, sessionGate, snapshotGate, expectReady = true } = {}) {
  const clock = timers();
  let hangNext = hang;
  if (!storage) { const values=new Map(); storage={getItem:k=>values.get(k)??null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)}; }
  const elements = new Map();
  const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const page = { $, errors: [], origin: (new Error().stack.split('\n')[2] || '').trim().replace(/^at /, '').replace(__dirname + path.sep, '') };
  pages.push(page);
  watchStatus($('status'), page);
  const document = { getElementById: $, createElement: tag => new Element(tag), createTextNode: text => Object.assign(new Element("#text"), {textContent:text}), addEventListener() {}, hidden: false, body: new Element('body'), querySelector: selector => selector === 'dialog[open]' ? null : $(selector), querySelectorAll: () => [] };
  $('target').value = 'slot2';
  const reads = [], uploads = [], sends = [], uploadGate = deferred(), windowEvents = {}, streamEvents = {}, streams = [], intervals = new Map();
  const sessions = []; let intervalID = 0, csrfToken = 'csrf';
  let failSend = true;
  const snapshot = {
    room: { id: 'room', name: 'Native', agents: { slot1: { runtime: 'codex' }, slot2: { runtime: 'claude' } } },
    relay: { sequence: 3, bindings: {}, audit: [], total_messages: 5000,
      messages: [{ id: 'last', state: 'human', from: 'slot1', to: 'user', text: 'complete reply' }] }
  };
  if(transport)Object.assign(snapshot.relay,transport);
  const fetch = async (url, options) => {
    if (url === 'api/v1/session') { sessions.push(url); if (sessionGate) { const gate=sessionGate; sessionGate=null; await gate.promise; } if (sessionFailures-- > 0) throw Error('service unavailable'); return response({ csrf_token: csrfToken }); }
    if (url.startsWith('api/v1/snapshot')) { reads.push(url); if (snapshotGate) { const gate=snapshotGate; snapshotGate=null; await gate.promise; } if (snapshotFailures-- > 0) throw Error('snapshot unavailable'); return response(snapshot); }
    if (url.startsWith('api/v1/pending')) return response({messages:[],total:0});
    if (url.startsWith('api/v1/sends/')) {if(receiptGate)await receiptGate.promise;const m=receipts.get(decodeURIComponent(url.split('/').pop()));return response({found:Boolean(m),message:m});}
    assert.equal(options.headers.get('X-PairRoom-CSRF'), csrfToken);
    if (url === 'api/v1/attachments') {
      uploads.push(options.body); await uploadGate.promise;
      return response(uploadFailure ? { error: 'invalid image' } : { id: 'image' }, uploadFailure ? 400 : 200);
    }
    if (url === 'api/v1/messages') {
      const payload=JSON.parse(options.body); sends.push(payload);
      receipts.set(payload.id,{from:'user',text:payload.text,to:payload.to,attachments:payload.attachment_ids.map(id=>({id})),review:payload.review});
      if (hangNext) {
        const stage = hangNext; hangNext = ''; failSend = false;
        const stalled = () => new Promise((_, reject) => {
          if (options.signal?.aborted) reject(Error('request aborted'));
          else options.signal?.addEventListener('abort', () => reject(Error('request aborted')), {once:true});
        });
        if (stage === 'headers') return stalled();
        return {ok:true, status:200, json:stalled};
      }
      if (failSend) { failSend = false; throw new Error('lost acceptance response'); }
      return response(receipts.get(payload.id));
    }
    throw new Error(`unexpected request ${url}`);
  };
  const context = vm.createContext({
    document, window: { PairRoomI18n: { t: (k, values) => values ? k + JSON.stringify(values) : k, apply() {}, lang: 'en' }, addEventListener(name, fn) { windowEvents[name] = fn; }, matchMedia: () => ({matches: false, addEventListener() {}}) },
    localStorage:storage,fetch, Headers, FormData, TextEncoder, crypto: webcrypto, URLSearchParams,
    navigator:{locks:manager}, AbortController, setTimeout:clock.setTimeout, clearTimeout:clock.clearTimeout,
    location: { hash: '', pathname: '/', search: '' }, history: { replaceState() {} },
    EventSource: class { constructor() { this.events={}; streams.push(this); } addEventListener(name, callback) { this.events[name]=callback; streamEvents[name]=callback; } close() { this.closed=true; } emit(name) { if (name === 'open') this.onopen?.(); else if (name === 'error') this.onerror?.(); else this.events[name]?.(); } },
    setInterval: callback => { const id=++intervalID; intervals.set(id,callback); return id; }, clearInterval: id => intervals.delete(id), console
  });
  for (const source of ['../internal/webui/assets/native-outbox.js','../internal/webui/assets/richtext.js','../internal/service/assets/native-host.js'])
    vm.runInContext(fs.readFileSync(path.join(__dirname,source),'utf8'),context);
  await tick();
  if (expectReady) {
    assert.ok(reads.length >= 1 && reads.every(p => p === 'api/v1/snapshot?tail=1'), 'unbounded history loaded during refresh');
    assert.equal($('message-count').textContent, '5000');
    assert.match($('messages').querySelector('.truncated-note').textContent, /1 \/ 5000$/);
  }
  if(draft)$('message-text').value = 'review';
  if(draft)$('attachment').files = [new File(['image bytes'], 'image.png', { type: 'image/png' })];
  const submit = () => $('composer').events.submit({ preventDefault() {} });
  const storageEvent = () => windowEvents.storage({ key: 'pairroom.native.outbox.v1.room' });
  return { $, reads, uploads, sends, sessions, streams, intervals, windowEvents, navigator:context.navigator,
    rotateSession() { csrfToken = 'csrf-recovered'; },
    failSnapshots(count=1) { snapshotFailures=count; }, blockSnapshot(gate) { snapshotGate=gate; }, poll() { for (const callback of intervals.values()) callback(); }, uploadGate, submit, storage, receipts, clock, storageEvent, async updateDelivery(delivery) { snapshot.relay.delivery=delivery; snapshot.relay.sequence++; streamEvents.native(); clock.expire(250); await tick(); } };
}

async function main() {
  // Temporary startup/restart failures recover through read-only requests.
  for (const failure of ['sessionFailures', 'snapshotFailures']) {
    const page = await fixture({[failure]:1,expectReady:false,draft:false});
    page.$('message-text').value = 'keep my draft';
    assert.equal(page.$('connection').dataset.state, 'reconnecting');
    assert.equal(page.clock.expire(500), 1, 'a failed first read must schedule recovery');
    await tick();
    assert.equal(page.streams.length, 1, 'startup did not recover without reload');
    page.streams[0].emit('open');
    assert.equal(page.$('connection').dataset.state, 'connected');
    assert.equal(page.$('message-text').value, 'keep my draft');
    assert.equal(page.sends.length, 0, 'reconnection must never publish');
  }
  // A late session/snapshot completion after leaving cannot resurrect the page.
  for (const blocked of ['sessionGate', 'snapshotGate']) {
    const gate = deferred();
    const page = await fixture({[blocked]:gate,expectReady:false,draft:false});
    page.windowEvents.pagehide({persisted:true});
    await tick();
    assert.equal(page.streams.length, 0, 'late startup created a stream after pagehide');
    assert.equal(page.intervals.size, 0, 'late startup created a polling timer after pagehide');
    page.windowEvents.pageshow({persisted:true});
    page.windowEvents.pageshow({persisted:true});
    await tick();
    assert.equal(page.streams.length, 1, 'repeated restore created duplicate streams');
    assert.equal(page.intervals.size, 1);
    page.streams[0].emit('open');
    gate.resolve(); await tick();
    assert.equal(page.streams.length, 1, 'obsolete startup completed after the newer page');
    assert.equal(page.$('connection').dataset.state, 'connected');
  }
  {
    const page = await fixture({draft:false});
    const first = page.streams[0]; first.emit('open');
    page.failSnapshots(); page.poll(); await tick();
    assert.equal(first.closed, true, 'failed fallback read retained a misleading live stream');
    assert.equal(page.$('connection').dataset.state, 'reconnecting');
    assert.equal(page.clock.expire(500), 1); await tick();
    assert.equal(page.streams.length, 2);
    page.streams[1].emit('open');
    first.emit('error');
    assert.equal(page.$('connection').dataset.state, 'connected', 'obsolete stream changed current state');
    assert.equal(page.intervals.size, 1, 'reconnect leaked the old polling timer');
    assert.equal(page.sends.length, 0);
    page.windowEvents.pagehide({persisted:true});
    assert.equal(page.clock.size, 0, 'leaving retained a refresh/reconnect/deadline timer');
    assert.equal(page.intervals.size, 0);
  }
  {
    const page = await fixture({sessionFailures:20,expectReady:false,draft:false});
    for (const delay of [500,1000,2000,4000,8000,15000,15000]) {
      assert.equal(page.clock.expire(delay), 1, 'connection recovery must use one bounded backoff');
      await tick();
      assert.equal(page.clock.size, 1, 'recovery scheduled duplicate timers');
    }
    page.windowEvents.pagehide({persisted:false});
    assert.equal(page.clock.size, 0, 'leaving a failed startup retained recovery');
  }
  {
    const page = await fixture({draft:false});
    page.streams[0].emit('open');
    page.navigator.onLine = false; page.windowEvents.offline();
    assert.equal(page.$('connection').dataset.state, 'offline');
    assert.equal(page.intervals.size, 0);
    page.$('message-text').value = 'offline draft';
    page.navigator.onLine = true;
    page.windowEvents.online(); page.windowEvents.online(); await tick();
    assert.equal(page.streams.length, 2, 'online events created duplicate streams');
    page.streams[1].emit('open');
    assert.equal(page.$('connection').dataset.state, 'connected');
    assert.equal(page.$('message-text').value, 'offline draft');
    assert.equal(page.sends.length, 0);
  }
  {
    // A successful publication waits for the in-flight refresh and its
    // follow-up snapshot instead of unlocking controls against stale state.
    const page = await fixture({draft:false});
    page.$('message-text').value = 'original'; await page.submit(); // response lost
    const gate = deferred(); page.blockSnapshot(gate); page.poll();
    const recovering = page.submit(); await tick();
    assert.equal(page.$('send').disabled, true, 'shared refresh returned before its snapshot');
    const reads = page.reads.length;
    gate.resolve(); await recovering;
    assert.equal(page.reads.length, reads + 1, 'acceptance requested a follow-up snapshot');
    assert.equal(page.$('send').disabled, false);
    assert.equal(page.sends.length, 2, 'only the explicit original-ID retry may publish');
  }
  {
    const page = await fixture({draft:false});
    page.$('message-text').value = 'uncertain original'; await page.submit();
    const original = JSON.stringify(page.sends[0]);
    page.rotateSession();
    page.streams[0].emit('error');
    assert.equal(page.clock.expire(500), 1); await tick();
    page.windowEvents.online(); page.windowEvents.pageshow({persisted:true}); await tick();
    assert.equal(page.sessions.length, 2, 'recovery must reacquire the browser session once');
    assert.equal(page.streams.length, 2);
    assert.equal(page.intervals.size, 1);
    assert.equal(page.sends.length, 1, 'session refresh replayed an uncertain publication');
    assert.equal(page.$('message-text').value, 'uncertain original');
    assert.equal(page.$('outbox').hidden, false);
    await page.submit(); // Explicit retry must use the renewed CSRF token.
    assert.equal(page.sends.length, 2);
    assert.equal(JSON.stringify(page.sends[1]), original, 'renewing session changed original-ID recovery');
    assert.equal(page.$('outbox').hidden, true);
  }
  const textTree = node => [node.textContent, ...node.children.map(textTree)].join(' ');
  const peerMessage = {id:'last',from:'slot1',to:'slot2',state:'unknown',text:'body',created_at:'2026-09-28T00:00:00Z'};
  const delivery = {queue_wait_ms:1250,reserved_at:'2026-09-28T00:00:00Z',inferred_outcome:{outcome:'submitted',at:'2026-09-28T00:00:01Z'},slot_observations:[{outcome:'suppressed',reason:'minimum_interval',at:'2026-09-28T00:00:00Z'}],slot_observation_count:5};
  const measured = await fixture({draft:false,transport:{messages:[peerMessage],delivery:{last:delivery}}});
  let visible = textTree(measured.$('messages'));
  assert.match(visible,/queueClaimTime.*1.25/,'unknown preserves queue-to-claim timing');
  assert.match(visible,/wakeInferredResult/,'slot-order association must be marked inferred');
  assert.match(visible,/wakeInferenceBoundary/,'result is not model acceptance');
  assert.match(visible,/slotWakeWhileQueued/,'suppression must stay a slot observation');
  assert.match(visible,/slotWakeWindow/,'truncated observation history must be visible');
  await measured.updateDelivery({last:{...delivery,queue_wait_ms:2000}});
  assert.match(textTree(measured.$('messages')),/queueClaimTime.*2/,'projection-only updates must rerender');
  for (const ms of [-1, null]) {
    await measured.updateDelivery({last:{queue_wait_ms:ms}});
    assert.doesNotMatch(textTree(measured.$('messages')),/queueClaimTime/,'invalid timing must not become a zero-second claim');
  }

  // HTML maxlength counts UTF-16 code units; the relay budget counts UTF-8 bytes.
  for (const text of ['x'.repeat(262145), '界'.repeat(87382), '😀'.repeat(65537)]) {
    const invalid = await fixture();
    invalid.$('message-text').value = text;
    invalid.uploadGate.resolve();
    await invalid.submit();
    assert.equal(invalid.uploads.length, 0, 'invalid text uploaded an unused image');
    assert.equal(invalid.sends.length, 0, 'oversized UTF-8 text reached publication');
    assert.match(invalid.$('status').textContent, /errors.request_too_large/);
    assert.equal(invalid.$('message-text').value, text, 'validation discarded the draft');
    for (const id of ['message-text', 'target', 'attachment', 'send']) assert.equal(invalid.$(id).disabled, false);
    invalid.$('message-text').value = 'corrected';
    await invalid.submit();
    await invalid.submit();
    assert.equal(invalid.uploads.length, 1);
    assert.equal(invalid.sends.length, 2);
    assert.deepEqual(invalid.sends[0], invalid.sends[1]);
    assert.equal(invalid.$('message-text').disabled, false);
  }
  for (const text of ['x'.repeat(262144), '界'.repeat(87381) + 'x', '😀'.repeat(65536)]) {
    assert.equal(Buffer.byteLength(text, 'utf8'), 262144);
    const valid = await fixture();
    valid.$('message-text').value = text;
    valid.$('attachment').files = [];
    await valid.submit();
    assert.equal(valid.sends.length, 1, 'the exact byte limit must remain valid');
    assert.equal(valid.sends[0].text, text, 'validation clipped a complete message');
    await valid.submit();
    assert.deepEqual(valid.sends[0], valid.sends[1]);
  }
  const f = await fixture();
  const first = f.submit();
  await tick();
  await f.submit(); // a disabled button alone does not guard a second submit event
  assert.equal(f.uploads.length, 1);
  assert.equal(f.sends.length, 0);
  for (const id of ['message-text', 'target', 'attachment', 'send']) assert.equal(f.$(id).disabled, true);
  f.uploadGate.resolve(); await first;
  assert.equal(f.sends.length, 1);
  assert.equal(f.$('send').disabled, false);
  for (const id of ['message-text', 'target', 'attachment']) assert.equal(f.$(id).disabled, true);
  assert.equal(f.$('message-text').value, 'review');
  await f.submit();
  assert.equal(f.uploads.length, 1, 'receipt recovery must not upload another attachment');
  assert.equal(f.sends.length, 2);
  assert.deepEqual(f.sends[0], f.sends[1], 'retry changed the stable ID or original payload');
  assert.deepEqual(f.sends[1].attachment_ids, ['image']);
  for (const id of ['message-text', 'target', 'attachment', 'send']) assert.equal(f.$(id).disabled, false);
  assert.equal(f.$('message-text').value, '');
  const bad = await fixture({ uploadFailure: true });
  const failed = bad.submit(); bad.uploadGate.resolve(); await failed;
  assert.equal(bad.sends.length, 0);
  assert.equal(bad.$('message-text').value, 'review');
  for (const id of ['message-text', 'target', 'attachment', 'send']) assert.equal(bad.$(id).disabled, false);
  const interrupted = await fixture(); interrupted.uploadGate.resolve(); await interrupted.submit();
  const reloaded = await fixture({storage:interrupted.storage,receipts:interrupted.receipts,draft:false});await tick();
  assert.equal(reloaded.sends.length,0,'refresh automatically re-published accepted work');
  assert.equal(reloaded.$('message-text').value,'');
  assert.equal(reloaded.$('outbox').hidden,true,'confirmed original receipt did not clear recovery draft');
  const noStorage=await fixture({storage:{getItem:()=>null,setItem(){throw Error('quota');},removeItem(){}}});noStorage.uploadGate.resolve();await noStorage.submit();
  assert.equal(noStorage.sends.length,0,'publication preceded durable browser draft');
  assert.equal(noStorage.$('message-text').disabled,false);
  // A hung response (including headers received but JSON body stalled) keeps
  // exactly the original durable payload and releases the composer on timeout.
  for (const stage of ['headers', 'body']) {
    const slow = await fixture({hang:stage}); slow.$('attachment').files=[];
    const pending = slow.submit(); await tick();
    assert.equal(slow.sends.length,1);
    assert.equal(slow.clock.expire(30000),1,'request/body has no bounded deadline');
    await pending;
    assert.equal(slow.$('status').textContent,'room.native.unknownSend room.native.requestTimeout','timeout exposed raw abort text');
    assert.equal(slow.$('send').disabled,false,'hung publication kept recovery locked');
    assert.equal(slow.sends.length,1,'timeout automatically repeated a publication');
    const saved = JSON.parse(slow.storage.getItem('pairroom.native.outbox.v1.room')).payload;
    assert.deepEqual(saved,slow.sends[0]);
    await slow.submit();
    assert.deepEqual(slow.sends[1],slow.sends[0],'explicit retry changed timed-out identity');
    assert.equal(slow.clock.size,0,'completed request leaked its timer');
  }
  // Read-only reconciliation and explicit Retry must not clear each other's
  // same-tab state while either operation is awaiting a response.
  const gate = deferred();
  const checking = await fixture({storage:interrupted.storage,receipts:interrupted.receipts,draft:false,receiptGate:gate});
  // The previous reload settled that record, so create a fresh interrupted send.
  checking.$('message-text').value='second';await checking.submit();
  const check = checking.$('outbox-check').events.click();await tick();
  assert.equal(checking.$('send').disabled,true,'receipt lookup races an enabled Retry');
  await checking.submit();assert.equal(checking.sends.length,1);
  gate.resolve();await check;await tick();
  assert.equal(checking.$('outbox').hidden,true);
  assert.equal(checking.sends.length,1,'receipt check published instead of only reading');

  const shared = locks();
  const tabA = await fixture({manager:shared});tabA.$('attachment').files=[];
  const tabB = await fixture({manager:shared,storage:tabA.storage});tabB.$('attachment').files=[];
  await shared.request('pairroom.native.outbox.v1.room',{mode:'exclusive',ifAvailable:true},async()=>{
    await tabB.submit();
    assert.equal(tabB.sends.length,0,'busy cross-tab lock allowed publication');
    assert.equal(tabB.$('status').textContent,'room.native.outboxBusy','a transient lock reported broken storage');
    assert.equal(tabB.$('send').disabled,false,'a transient lock blocked a later attempt');
  });
  await Promise.all([tabA.submit(),tabB.submit()]);
  assert.equal(tabA.sends.length+tabB.sends.length,1,'concurrent tabs published two unconfirmed drafts');
  // Another window's unconfirmed send must never replace a draft typed here;
  // its later settlement would otherwise clear this composer.
  const owner = await fixture({draft:false});
  const typing = await fixture({storage:owner.storage,receipts:owner.receipts,draft:false});
  typing.$('message-text').value = 'my own draft';
  owner.$('message-text').value = 'owner send'; await owner.submit(); // response lost: record kept
  typing.storageEvent(); await tick();
  assert.equal(typing.$('message-text').value, 'my own draft', 'another window replaced a typed draft');
  assert.equal(typing.$('outbox').hidden, false, 'a pending send from another window is not signalled');
  assert.equal(typing.$('outbox-notice').textContent, 'room.native.foreignPending');
  await typing.submit();
  assert.equal(typing.sends.length, 0, 'a second window published beside an unconfirmed send');
  assert.equal(typing.$('status').textContent, 'room.native.foreignPending');
  await owner.submit(); // explicit retry settles and clears the record
  typing.storageEvent(); await tick();
  assert.equal(typing.$('outbox').hidden, true);
  assert.equal(typing.$('message-text').value, 'my own draft', 'settlement elsewhere cleared this draft');
  await typing.submit();
  assert.equal(typing.sends.length, 1, 'the draft can be sent once the other window settles');
  // An idle window still adopts the record so either window can recover it.
  const sender = await fixture({draft:false});
  const idle = await fixture({storage:sender.storage,receipts:new Map(),draft:false});
  sender.$('message-text').value = 'adopt me'; await sender.submit();
  idle.storageEvent(); await tick();
  assert.equal(idle.$('message-text').value, 'adopt me');
  assert.equal(idle.$('send').textContent, 'room.native.retryOriginal');
  const unsupported = await fixture({manager:null});unsupported.$('attachment').files=[];
  await unsupported.submit();assert.equal(unsupported.sends.length,0,'missing Web Locks allowed unsafe send');
  console.log('Native UI: lifecycle recovery, bounded reads, accurate totals, UTF-8 validation, single submission and immutable retry payload passed.');
}
main().catch(error => { console.error(error); reportPages(); process.exitCode = 1; });
