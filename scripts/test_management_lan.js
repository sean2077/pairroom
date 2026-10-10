'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
async function flush() { for (let i = 0; i < 12; i++) await Promise.resolve(); }

function fixture() {
  const nodes = new Map(), writes = [], storage = new Map(), copied = [], confirmations = [];
  let epoch = 0, uuid = 0, handler = async () => ({});
  class Element {
    constructor(tag, properties = {}) {
      this.tag = tag; this.children = []; this.value = ''; this.open = false; this.hidden = false; this.disabled = false;
      Object.assign(this, properties);
      if (this.id) nodes.set(this.id, this);
    }
    append(...children) { this.children.push(...children.flat().filter(value => value !== null && value !== undefined && value !== false)); }
    replaceChildren(...children) { this.children = []; this.append(...children); }
    setAttribute(key, value) { this[key] = value; }
    async click() { if (!this.disabled) return this.onClick?.(); }
  }
  const node = (tag, properties, ...children) => { const value = new Element(tag, properties); value.append(...children); return value; };
  const get = id => { if (!nodes.has(id)) nodes.set(id, new Element('div', { id })); return nodes.get(id); };
  const localStorage = { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) };
  const sandbox = { window: {}, document: { getElementById: get }, AbortController, setTimeout, clearTimeout, TextEncoder, localStorage,
    navigator: { locks: { request: async (_key, _options, callback) => callback({}) } },
    crypto: { randomUUID: () => `fixture-${++uuid}` } };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync('internal/webui/assets/native-outbox.js', 'utf8'), sandbox);
  vm.runInContext(fs.readFileSync('internal/service/assets/management-lan.js', 'utf8'), sandbox);
  const api = async (path, options) => {
    const request = { path, method: options.method, body: options.body ? JSON.parse(options.body) : undefined };
    writes.push(request); return handler(request);
  };
  const t = key => key;
  const client = sandbox.window.PairRoomLAN.create({ t, node, api,
    actionButton: (textContent, onClick, className, disabled = false) => node('button', { textContent, onClick, className, disabled }),
    settingsPanel: (title, subtitle, ...children) => node('section', { title, subtitle }, ...children),
    settingRow: (title, subtitle, control) => node('div', { title, subtitle }, control),
    copyText: value => copied.push(value),
    showDialog: id => { get(id).open = true; }, closeDialog: id => { get(id).open = false; },
    confirm: value => confirmations.push(value), refresh: async () => {}, renderSettings: () => {},
    getSnapshot: () => ({ joined_rooms: [] }), isSettings: () => false,
    isCurrent: () => { const generation = epoch; return () => generation === epoch; },
  });
  function all(root = get('lan-room-body')) { return [root, ...root.children.filter(child => child instanceof Element).flatMap(all)]; }
  function button(key) { const value = all().find(item => item.tag === 'button' && item.textContent === key); assert.ok(value, `button ${key}`); return value; }
  return { client, get, writes, storage, copied, confirmations, all, button,
    setHandler: callback => { handler = callback; }, invalidate: () => { epoch++; client.reset(); } };
}

async function main() {
  {
    const f = fixture();
    f.setHandler(async request => request.path.endsWith('/lan') ? { pending: [{ label: 'A familiar name', request_id: 'untrusted-request', fingerprint: 'f'.repeat(64), runtime: 'codex' }] } : {});
    await f.client.openRoom({ id: 'host-room', name: 'Shared Room' });
    assert.equal(f.get('lan-accept-receipt').value, '', 'pending rows must never supply their own approval receipt');
    await f.button('room.lan.accept').click();
    f.get('lan-accept-receipt').value = 'accept the first person';
    await f.button('room.lan.accept').click();
    assert.equal(f.writes.filter(request => request.method === 'POST').length, 0, 'name or invalid receipt must not trigger acceptance');
    f.get('lan-accept-receipt').value = 'pairroom-accept:trusted_peer_receipt';
    await f.button('room.lan.accept').click();
    const accepted = f.writes.find(request => request.path.endsWith('/accept'));
    assert.deepEqual(accepted.body, { receipt: 'pairroom-accept:trusted_peer_receipt' });
  }
  {
    const f = fixture(), reply = deferred();
    f.setHandler(request => request.path.endsWith('/invite') ? reply.promise : { pending: [] });
    await f.client.openRoom({ id: 'host-room', name: 'Shared Room' });
    const button = f.button('room.lan.createInvite');
    const first = button.click();
    await button.click();
    assert.equal(f.writes.filter(request => request.path.endsWith('/invite')).length, 1, 'parallel clicks must not issue two invitations');
    reply.resolve({ invite: "pairroom://join/a'; unexpected-shell-command", expires_at: '2099-01-01T00:00:00Z' });
    await first;
    assert.equal(f.get('lan-invite-output').hidden, true, 'a malformed descriptor must not become a shell command');
    assert.equal(f.copied.length, 0);
  }
  {
    const f = fixture(), reply = deferred();
    f.setHandler(() => reply.promise);
    const pending = f.client.openRoom({ id: 'old-room', name: 'Old owner session' });
    f.invalidate();
    reply.resolve({ pending: [{ request_id: 'old-request' }] });
    await pending;
    assert.equal(f.get('lan-room-dialog').open, false);
    assert.deepEqual(f.get('lan-room-body').children, [], 'late old-session reads cannot repopulate the new owner view');
  }
  {
    const f = fixture();
    f.client.openJoined({ id: 'lan_pending', name: 'Pending admission', status: 'pending', connected: false, endpoint: 'https://192.168.1.2:8877' });
    await flush();
    assert.equal(f.writes.length, 0, 'opening a pending association must not read the host inbox');
    assert.equal(f.button('room.lan.leave').hidden, true, 'pending admission cannot promise a confirmed remote leave');
    assert.equal(f.button('room.lan.detach').hidden, false);
    await f.button('room.lan.detach').click();
    assert.equal(f.writes.length, 0, 'local detach needs its explicit confirmation');
    assert.equal(f.confirmations.length, 1);
    assert.equal(f.confirmations[0].message, 'room.lan.detachHelp');
    await f.confirmations[0].action();
    assert.deepEqual(f.writes, [{ path: '/api/v1/lan/joined/lan_pending/detach', method: 'POST', body: {} }]);
    assert.equal(f.get('lan-room-dialog').open, false);
  }
  {
    const f = fixture();
    let first = true, accepted;
    f.setHandler(request => {
      if (request.path.endsWith('/history')) return { messages: [] };
      if (request.path.endsWith('/send')) {
        assert.ok(f.storage.size, 'publication recovery identity must be stored before the network request');
        accepted ||= { id: 'server-message', from: 'user', to: request.body.to, text: request.body.text, attachments: [] };
        if (first) { first = false; throw new Error('response lost'); }
        return accepted;
      }
      return {};
    });
    f.client.openJoined({ id: 'lan_fixture', name: 'Remote Room', status: 'accepted', connected: true, endpoint: 'https://192.168.1.2:8877' });
    await flush();
    const text = f.all().find(item => item.tag === 'textarea');
    const target = f.all().find(item => item.tag === 'select');
    text.value = 'Evidence to the owner'; target.value = 'slot2';
    await f.button('room.native.send').click();
    assert.equal(f.storage.size, 1, 'response loss must retain the original publication');
    assert.equal(text.disabled, true, 'an uncertain publication cannot be edited under its original ID');
    await flush();
    assert.equal(f.writes.filter(request => request.path.endsWith('/send')).length, 1, 'history reads do not automatically republish');
    await f.button('room.native.retryOriginal').click();
    const sends = f.writes.filter(request => request.path.endsWith('/send'));
    assert.equal(sends.length, 2);
    assert.deepEqual(sends[1].body, sends[0].body, 'an explicit retry retains the exact client ID, body and target');
    assert.equal(f.storage.size, 0);
  }
  console.log('LAN UI: explicit trusted receipts, single invite issuance, safe descriptors, session isolation, pending local detach and original-ID publication recovery: ok');
}
main().catch(error => { console.error(error); process.exitCode = 1; });
