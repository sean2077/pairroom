'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../internal/enterpriseservice/web/app.js'), 'utf8');
const flush = () => new Promise((resolve) => setImmediate(resolve));
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
class Element {
  constructor() { this.listeners = new Map(); this.children = []; this.textContent = ''; this.hidden = false; this.disabled = false; this.value = ''; }
  addEventListener(name, fn) { this.listeners.set(name, fn); }
  setAttribute(name, value) { this[name] = value; }
  append(...values) { this.children.push(...values); }
  replaceChildren(...values) { this.children = values; }
  fire(name) { return this.listeners.get(name)({ preventDefault() {} }); }
  click() { if (this.listeners.has('click')) return this.fire('click'); }
  text() { return this.textContent + this.children.map((value) => value.text()).join(''); }
  set innerHTML(_value) { throw new Error('private data must use textContent'); }
}
class Storage {
  constructor() { this.values = new Map(); }
  get length() { return this.values.size; }
  key(index) { return [...this.values.keys()][index]; }
  getItem(key) { return this.values.get(key) || null; }
  setItem(key, value) { this.values.set(key, value); }
  removeItem(key) { this.values.delete(key); }
}
class Hub {
  constructor() { this.channels = []; this.sent = []; }
  implementation() {
    const hub = this;
    return class {
      constructor(name) { this.name = name; hub.channels.push(this); }
      postMessage(data) { hub.sent.push(data); for (const peer of hub.channels) if (peer !== this && peer.name === this.name) queueMicrotask(() => { if (peer.onmessage) peer.onmessage({ data }); }); }
    };
  }
}
const session = (user = 'user-a', id = 'session-a') => ({ user_id: user, session_id: id, deployment_id: 'deployment-1', csrf: 'private-csrf-' + id, administrator: false });
const history = (id = 'room-a', text = 'private-room-a') => ({ room_id: id, revision: 1, sequence: 1, total_messages: 1, total_audit: 0, bindings: {}, messages: [{ id: 'message-1', from: 'user', to: 'slot1', state: 'queued', text }] });
const headers = (identity) => ({ get: (name) => ({ 'X-PairRoom-User': identity.user_id, 'X-PairRoom-Session': identity.session_id })[name] || null });
function fixture(options = {}) {
  const elements = new Map(), requests = [], storage = options.storage || new Storage(), hub = options.hub || new Hub(), navigations = [];
  let identity = options.session || session(), nextID = 0;
  const element = (id) => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  element('rooms-state').value = 'active'; element('collaboration-mode').value = 'default'; element('member-role').value = 'member'; element('send-target').value = 'slot1'; element('attachment-kind').value = 'file';
  function complete(request, status, data, chosen = identity) {
    request.resolved = true; request.resolve({ status, ok: status >= 200 && status < 300, headers: headers(chosen), json: async () => data });
  }
  const globals = {
    document: { getElementById: element, createElement: () => new Element() },
    window: { location: { origin: 'https://service.example.test', assign: (url) => navigations.push(url) }, localStorage: storage },
    BroadcastChannel: hub.implementation(), AbortController, URL, URLSearchParams, Date, Blob, TextEncoder, queueMicrotask,
    crypto: { randomUUID: () => 'client-' + (++nextID), subtle: require('node:crypto').webcrypto.subtle },
    // Ignoring cancellation is intentional: account/lane guards must reject
    // stale fetch and JSON results even when a transport cannot cancel them.
    fetch: (url, requestOptions) => {
      const request = { url, options: requestOptions, resolved: false, ...deferred() }; requests.push(request);
      if (!requestOptions.method && url.includes('/catalog/') && options.autoCatalog !== false) {
        const parsed = new URL(url, 'https://service.example.test'), resource = parsed.pathname.split('/').at(-1);
        let data = { resource };
        if (resource === 'room') data.rooms = [{ room_id: parsed.searchParams.get('room_id'), name: parsed.searchParams.get('room_id'), role: 'owner', owner_user_id: identity.user_id, archived: false, revision: 1 }];
        complete(request, 200, data);
      }
      return request.promise;
    }
  };
  vm.runInNewContext(source, globals);
  const take = (suffix) => { const result = requests.find((value) => !value.resolved && value.url.endsWith(suffix)); assert.ok(result, 'pending request ' + suffix); return result; };
  const reply = (suffix, status, data, chosen) => complete(take(suffix), status, data, chosen);
  return { element, requests, storage, hub, navigations, take, reply, complete, globals, identity: () => identity, setIdentity: (value) => { identity = value; }, text: () => [...elements.values()].map((value) => value.text()).join('') };
}
async function signIn(f = fixture(), value = session()) { f.setIdentity(value); f.reply('/session', 200, value, value); await flush(); return f; }
async function open(f, id = 'room-a', text = 'private-room-a') { f.element('room-id').value = id; const operation = f.element('room-form').fire('submit'); await flush(); f.reply('/rooms/' + id, 200, history(id, text)); await operation; await flush(); return f; }
module.exports = { Element, Storage, Hub, session, history, headers, fixture, signIn, open, deferred, flush };
