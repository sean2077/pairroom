'use strict';

// Exercise production transport/state transitions without timing-dependent DOM
// rendering. The companion browser contract covers the actual visible surfaces.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function client() {
  const nodes = new Map(), renders = [], notices = [], events = [];
  const document = {
    hidden: false, activeElement: null, body: {dataset: {}},
    querySelectorAll() { return []; }, querySelector() { return null; },
    addEventListener() {},
    getElementById(id) {
      if (!nodes.has(id)) nodes.set(id, {
        value: '', checked: false, disabled: false, textContent: '', style: {}, dataset: {}, attributes: {},
        listeners: new Map(), hidden: false,
        addEventListener(type, cb) { this.listeners.set(type, cb); },
        setAttribute(key, value) { this.attributes[key] = value; },
        removeAttribute(key) { delete this.attributes[key]; },
        setCustomValidity() {}, replaceChildren() {}, querySelectorAll() { return []; }, querySelector() { return null; },
        focus() { document.activeElement = this; }, close() { this.open = false; },
        classList: {add() {}, remove() {}, toggle() {}, contains() { return false; }},
      });
      return nodes.get(id);
    },
  };
  const location = {hash: '#/overview', pathname: '/', search: '', origin: 'http://127.0.0.1:9876'};
  const sandbox = {
    document, location, history: {replaceState() {}}, console, URL, URLSearchParams, Headers, Response,
    window: {location, localStorage: {getItem() { return null; }}, addEventListener() {}, dispatchEvent(e) { events.push(e); }},
    requestAnimationFrame() {}, setTimeout: () => 1, clearTimeout() {}, queueMicrotask,
    Event: class {constructor(type) { this.type = type; }},
    fetch: async () => { throw new Error('unexpected fetch'); }, renders, notices,
  };
  const source = fs.readFileSync('internal/service/assets/management.js', 'utf8');
  const marker = "  loadStoredTheme();\n  window.addEventListener('storage'";
  assert.ok(source.includes(marker), 'keep boot interception explicit');
  const hook = `
    globalThis.management = {state, api, refresh, loadAgentCatalog, withBusy,
      syncConfirmRequirement, createBrowserSession, showCredentialLogin,
      invalidateSessionReads, openRoomInBrowserAction, connect,
      setAPI(callback) { api = callback; }, setCanRender(value) { canRenderNow = () => value; }};
    render = () => { renders.push(state.snapshot); state.renderedSnapshotKey = snapshotRenderKey(state.snapshot); };
    toast = (...args) => notices.push(args);
    updateChrome = setDisconnected = applyPreferences = scheduleRefresh = renderLoading = () => {};
    connect = async () => {};
  `;
  vm.runInNewContext(source.replace(marker, hook + marker), sandbox);
  const c = sandbox.management;
  c.state.authenticated = true;
  c.state.csrfToken = 'current-session';
  c.state.snapshot = {rooms: [], runtimes: [], projects: []};
  c.setCanRender(true);
  return {...c, nodes, renders, notices, events, setFetch(callback) { sandbox.fetch = callback; }};
}

async function flush() { for (let i = 0; i < 8; i++) await Promise.resolve(); }

async function main() {
  {
    const c = client(), oldRead = deferred(), newRead = deferred(); let reads = 0;
    c.setAPI(() => (++reads === 1 ? oldRead : newRead).promise);
    const poll = c.refresh();
    const afterWrite = c.refresh({fresh: true, forceRender: true, notify: true});
    assert.equal(poll, afterWrite, 'post-write caller joins the read loop, not a separate storm');
    oldRead.resolve({rooms: [{id: 'stale', lifecycle: 'active'}]});
    await flush();
    assert.equal(reads, 2, 'post-write refresh must read after the completed mutation');
    assert.equal(c.renders.length, 0, 'pre-mutation data must not flash in the UI');
    newRead.resolve({rooms: [{id: 'created', lifecycle: 'active'}]});
    await afterWrite;
    assert.equal(c.state.snapshot.rooms[0].id, 'created');
    assert.equal(c.renders.length, 1);
    assert.equal(c.notices.length, 1, 'options from coalesced callers are not lost');
    assert.equal(c.state.refreshPromise, null);
  }
  {
    const c = client(), read = deferred(); let reads = 0;
    c.setCanRender(false);
    c.setAPI(() => { reads++; return read.promise; });
    const first = c.refresh();
    c.refresh({forceRender: true});
    for (let i = 0; i < 30; i++) c.refresh();
    read.resolve({rooms: []}); await first;
    assert.equal(reads, 1, 'ordinary reads still coalesce');
    assert.equal(c.renders.length, 1, 'manual force-render survives an existing poll');
  }
  {
    const c = client(), oldRead = deferred(), newRead = deferred(); let reads = 0;
    c.setAPI(() => (++reads === 1 ? oldRead : newRead).promise);
    const old = c.refresh();
    c.showCredentialLogin();
    c.state.authenticated = true;
    const current = c.refresh();
    oldRead.resolve({rooms: [{id: 'old-session'}]}); await old;
    assert.equal(c.state.refreshPromise, current, 'old session must not clear a new in-flight request');
    assert.equal(c.state.snapshot, null);
    newRead.resolve({rooms: [{id: 'new-session'}]}); await current;
    assert.equal(c.state.snapshot.rooms[0].id, 'new-session');
  }
  {
    const c = client(), response = deferred();
    c.setFetch(() => response.promise);
    const old = c.api('/api/v1/service');
    c.invalidateSessionReads();
    c.state.csrfToken = 'new-session';
    response.resolve(new Response('{"error":"expired"}', {status: 401}));
    await assert.rejects(old, /expired/);
    assert.equal(c.state.authenticated, true, 'an old 401 cannot log out a newer session');
    assert.equal(c.state.csrfToken, 'new-session');
  }
  {
    const c = client(), oldRead = deferred(), forced = deferred(); let reads = 0;
    c.setAPI(() => (++reads === 1 ? oldRead : forced).promise);
    const old = c.loadAgentCatalog();
    const newer = c.loadAgentCatalog(true);
    oldRead.resolve({revision: 1}); await flush();
    assert.equal(c.state.agentCatalog, null, 'superseded catalog must not repopulate old providers');
    forced.resolve({revision: 2});
    assert.equal((await old).revision, 2);
    assert.equal((await newer).revision, 2);
    assert.equal(c.state.agentCatalog.revision, 2);
    assert.equal(c.state.agentCatalogPromise, null);
  }
  {
    const c = client(), pending = deferred();
    c.setAPI(() => pending.promise);
    const catalog = c.loadAgentCatalog();
    c.invalidateSessionReads();
    pending.resolve({revision: 'obsolete'});
    await assert.rejects(catalog, /Obsolete/);
    assert.equal(c.state.agentCatalog, null, 'old authenticated catalog cannot repopulate a new session');
  }
  {
    const c = client(), write = deferred(); let calls = 0;
    c.syncConfirmRequirement();
    const button = c.nodes.get('confirm-submit');
    button.textContent = 'Confirm';
    const task = c.withBusy(button, () => { calls++; return write.promise; });
    c.syncConfirmRequirement();
    assert.equal(button.disabled, true, 'confirmation input updates must not unlock a pending mutation');
    button.disabled = false; // Even an unrelated renderer cannot bypass the ownership guard.
    await c.withBusy(button, async () => { calls++; });
    assert.equal(calls, 1);
    write.resolve(); await task;
    assert.equal(button.attributes['aria-busy'], 'false');
    assert.equal(button.textContent, 'Confirm');
  }
  {
    const c = client(), activation = deferred(); let activations = 0, opens = 0;
    c.state.snapshot.rooms = [{id: 'r', lifecycle: 'active', bindings: {claude: {mode: 'new'}, codex: {mode: 'new'}}}];
    c.state.snapshot.runtimes = [{room_id: 'r', phase: 'suspended'}];
    c.setAPI((path) => {
      if (path.endsWith('/activate')) { activations++; return activation.promise; }
      if (path.endsWith('/open-browser')) { opens++; return Promise.resolve({opened: true}); }
      if (path === '/api/v1/service') return Promise.resolve(c.state.snapshot);
      throw new Error('unexpected path ' + path);
    });
    const opening = c.openRoomInBrowserAction('r');
    await c.openRoomInBrowserAction('r');
    activation.resolve({phase: 'active'});
    await opening;
    assert.equal(activations, 1, 'repeated clicks must not open duplicate native browser windows');
    assert.equal(opens, 1, 'readiness comes from activation, even if the polling snapshot is older');
    assert.equal(c.state.openingBrowsers.size, 0);
  }
  {
    const c = client(); let clock = 1, busy = true;
    c.setAPI(async () => ({rooms: [], generated_at: String(clock), runtimes: [{room_id: 'r', busy, last_used_at: String(clock)}]}));
    await c.refresh();
    clock++;
    await c.refresh();
    assert.equal(c.renders.length, 1, 'busy telemetry timestamps must not rebuild unchanged views');
    busy = false;
    await c.refresh();
    clock++;
    await c.refresh();
    assert.equal(c.renders.length, 3, 'idle activity timestamps remain meaningful');
  }
  console.log('management-client freshness, session isolation, catalog, and mutation ownership: ok');
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
