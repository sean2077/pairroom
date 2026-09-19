'use strict';
// Execute the shipped client, not a second implementation of its state machine.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');

class Element {
  constructor() {
    this.children = []; this.events = {}; this.dataset = {}; this.className = '';
    this.value = ''; this.textContent = ''; this.files = []; this.options = [];
    this.disabled = false; this.scrollHeight = 10; this.scrollTop = 0; this.clientHeight = 10;
    this.classList = { toggle() {} };
  }
  append(...nodes) { for (const node of nodes) { node.parent = this; this.children.push(node); } }
  prepend(node) { node.parent = this; this.children.unshift(node); }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter(n => n !== this); }
  querySelectorAll(selector) { return this.children.filter(n => n.className.split(' ').includes(selector.slice(1))); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  addEventListener(name, fn) { this.events[name] = fn; }
  setAttribute() {}
}
const tick = () => new Promise(resolve => setImmediate(resolve));
const response = (data, status = 200) => ({ ok: status < 400, status, json: async () => data });
function deferred() { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; }

async function fixture({ uploadFailure = false } = {}) {
  const elements = new Map();
  const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const document = { getElementById: $, createElement: () => new Element(), addEventListener() {}, hidden: false };
  $('target').value = 'slot2';
  const reads = [], uploads = [], sends = [], uploadGate = deferred();
  let failSend = true;
  const snapshot = {
    room: { id: 'room', name: 'Native', agents: { slot1: { runtime: 'codex' }, slot2: { runtime: 'claude' } } },
    relay: { sequence: 3, bindings: {}, audit: [], total_messages: 5000,
      messages: [{ id: 'last', state: 'human', from: 'slot1', to: 'user', text: 'complete reply' }] }
  };
  const fetch = async (url, options) => {
    if (url === 'api/v1/session') return response({ csrf_token: 'csrf' });
    if (url.startsWith('api/v1/snapshot')) { reads.push(url); return response(snapshot); }
    assert.equal(options.headers.get('X-PairRoom-CSRF'), 'csrf');
    if (url === 'api/v1/attachments') {
      uploads.push(options.body); await uploadGate.promise;
      return response(uploadFailure ? { error: 'invalid image' } : { id: 'image' }, uploadFailure ? 400 : 200);
    }
    if (url === 'api/v1/messages') {
      sends.push(JSON.parse(options.body));
      if (failSend) { failSend = false; throw new Error('lost acceptance response'); }
      return response({ accepted: true });
    }
    throw new Error(`unexpected request ${url}`);
  };
  const context = vm.createContext({
    document, window: { PairRoomI18n: { t: k => k, apply() {}, lang: 'en' }, addEventListener() {} },
    fetch, Headers, FormData, crypto: webcrypto, URLSearchParams,
    location: { hash: '', pathname: '/', search: '' }, history: { replaceState() {} },
    EventSource: class { addEventListener() {} close() {} }, setInterval: () => 1, clearInterval() {}, console
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../internal/service/assets/native-host.js'), 'utf8'), context);
  await tick();
  assert.deepEqual(reads, ['api/v1/snapshot?tail=1']);
  assert.equal($('message-count').textContent, '5000');
  assert.match($('messages').querySelector('.truncated-note').textContent, /1 \/ 5000$/);
  $('message-text').value = 'review';
  $('attachment').files = [new File(['image bytes'], 'image.png', { type: 'image/png' })];
  const submit = () => $('composer').events.submit({ preventDefault() {} });
  return { $, reads, uploads, sends, uploadGate, submit };
}

async function main() {
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
  console.log('Native UI: bounded reads, accurate totals, single submission and immutable retry payload passed.');
}
main().catch(error => { console.error(error); process.exitCode = 1; });
