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
  const nodes = new Map(), renders = [], notices = [], events = [], timers = new Map();
  let timerID = 0;
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
    requestAnimationFrame() {}, queueMicrotask,
    setTimeout(callback, delay) { const id = ++timerID; timers.set(id, {callback, delay}); return id; },
    clearTimeout(id) { timers.delete(id); },
    setInterval(callback, delay) { const id = ++timerID; timers.set(id, {callback, delay, interval: true}); return id; },
    clearInterval(id) { timers.delete(id); },
    Event: class {constructor(type) { this.type = type; }},
    fetch: async () => { throw new Error('unexpected fetch'); }, renders, notices,
  };
  const source = fs.readFileSync('internal/service/assets/management.js', 'utf8');
  const marker = "  loadStoredTheme();\n  window.addEventListener('storage'";
  assert.ok(source.includes(marker), 'keep boot interception explicit');
  const hook = `
    globalThis.management = {state, api, refresh, loadAgentCatalog, withBusy, scheduleRefresh,
      syncConfirmRequirement, submitConfirm, resetConfirmState, createBrowserSession, showCredentialLogin,
      invalidateSessionReads, openRoomInBrowserAction, connect, updateDesktopStartup,
      setDesktop(value) { window.PairRoomDesktop = value; },
      setAPI(callback) { api = callback; }, setCanRender(value) { canRenderNow = () => value; }};
    render = () => { renders.push(state.snapshot); state.renderedSnapshotKey = snapshotRenderKey(state.snapshot); };
    toast = (...args) => notices.push(args);
    updateChrome = setDisconnected = applyPreferences = renderLoading = () => {};
    connect = async () => {};
  `;
  vm.runInNewContext(source.replace(marker, hook + marker), sandbox);
  const c = sandbox.management;
  c.state.authenticated = true;
  c.state.csrfToken = 'current-session';
  c.state.snapshot = {rooms: [], runtimes: [], projects: []};
  c.setCanRender(true);
  timers.clear();
  return {...c, nodes, getNode: id => document.getElementById(id), timers, document, renders, notices, events, setFetch(callback) { sandbox.fetch = callback; }};
}

async function flush() { for (let i = 0; i < 8; i++) await Promise.resolve(); }

async function main() {
  {
    const c = client();
    c.state.tabs = ['r'];
    c.state.snapshot.rooms = [{id: 'r', lifecycle: 'active'}];
    c.state.snapshot.runtimes = [{room_id: 'r', phase: 'starting'}];
    // Completion of a 202 activation must not wait for the ordinary 10s poll,
    // including when the user disables background auto-refresh.
    c.state.preferences.refreshMs = 0;
    c.scheduleRefresh();
    assert.equal(c.timers.size, 1, 'pending activation still needs a readiness read');
    const [id, timer] = [...c.timers][0];
    assert.ok(timer.delay > 0 && timer.delay <= 1000, 'show readiness promptly');
    let reads = 0;
    c.setAPI(async () => {
      reads++;
      return {rooms: [{id: 'r', lifecycle: 'active'}], runtimes: [{room_id: 'r', phase: 'active'}]};
    });
    c.timers.delete(id);
    await timer.callback();
    await flush();
    assert.equal(reads, 1);
    assert.equal(c.timers.size, 0, 'completed activation respects disabled auto-refresh');
  }
  {
    const c = client();
    c.state.tabs = ['r'];
    c.state.snapshot.rooms = [{id: 'r', lifecycle: 'active'}];
    c.state.snapshot.runtimes = [{room_id: 'r', phase: 'queued'}];
    c.scheduleRefresh();
    c.scheduleRefresh();
    assert.equal(c.timers.size, 1, 'only one refresh timer, even during queue polling');
    assert.ok([...c.timers.values()][0].delay <= 1000);
    c.state.snapshot.runtimes[0].phase = 'failed';
    c.scheduleRefresh();
    assert.equal([...c.timers.values()][0].delay, 10000, 'failure returns to ordinary polling, not automatic retries');
    c.document.hidden = true;
    c.scheduleRefresh();
    assert.equal(c.timers.size, 0, 'hidden pages do not keep polling');
    c.document.hidden = false;
    c.state.authenticated = false;
    c.scheduleRefresh();
    assert.equal(c.timers.size, 0, 'signed-out pages do not keep polling');
  }
  {
    const c = client(), pending = deferred();
    const button = c.getNode('confirm-submit');
    const dialog = c.getNode('confirm-dialog');
    dialog.open = true;
    button.textContent = 'Archive';
    c.state.confirmAction = () => pending.promise;
    const submitting = c.submitConfirm({ preventDefault() {} });
    c.resetConfirmState(); // Escape, then open a different confirmation.
    const replacement = async () => {};
    c.state.confirmAction = replacement;
    dialog.open = true;
    button.textContent = 'Delete a different Room';
    pending.resolve();
    await submitting;
    assert.equal(dialog.open, true, 'old action completion must not close a new confirmation');
    assert.equal(c.state.confirmAction, replacement, 'new action ownership must survive');
    assert.equal(button.textContent, 'Delete a different Room', 'old busy label must not overwrite a new action');
  }
  {
    const c = client(), update = deferred(); let writes = 0;
    c.setDesktop({readStartup: async () => false, setStartup: () => { writes++; return update.promise; }});
    await c.updateDesktopStartup();
    assert.equal(c.state.desktopStartup.enabled, false);
    const write = c.updateDesktopStartup(true);
    assert.equal(c.state.desktopStartup.pending, true);
    await c.updateDesktopStartup(false);
    assert.equal(writes, 1, 'coalesce native setting changes while pending');
    update.reject(Object.assign(new Error('access denied'), {enabled: false}));
    await write;
    assert.equal(c.state.desktopStartup.enabled, false, 'failed toggle shows actual native state, not optimistic success');
    assert.equal(c.state.desktopStartup.pending, false);
    assert.equal(c.state.desktopStartup.error, 'access denied');
    c.setDesktop({readStartup: async () => { throw new Error('unavailable'); }});
    await c.updateDesktopStartup();
    assert.equal(c.state.desktopStartup.enabled, null, 'unknown OS state disables the toggle');
  }

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
