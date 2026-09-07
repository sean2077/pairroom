'use strict';

// Execute the real Room client in a minimal DOM. Only visual rendering and the
// boot side effect are replaced; transport, composer, storage, and listeners are
// the production implementation. No browser or runtime dependency is required.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function client() {
  const nodes = new Map(), timers = new Map(), storage = new Map(), notices = [], renders = [], lifecycle = new Map(), revoked = [];
  let timerID = 0;
  const document = { body: { dataset: {} }, hidden: false, activeElement: null,
    addEventListener() {}, querySelectorAll() { return []; }, querySelector() { return null; },
    getElementById(id) {
      if (!nodes.has(id)) nodes.set(id, {
        value: '', dataset: {}, style: {}, listeners: new Map(), attributes: {},
        scrollHeight: 300, scrollTop: 0, clientHeight: 100,
        addEventListener(type, callback) { this.listeners.set(type, callback); },
        setAttribute(key, value) { this.attributes[key] = value; },
        removeAttribute(key) { delete this.attributes[key]; },
        reportValidity() { return true; },
        querySelector() { return null; },
        focus() { document.activeElement = this; },
        classList: { add() {}, remove() {}, toggle() {} },
      });
      return nodes.get(id);
    },
  };
  class EventSource {
    constructor(url) { this.url = url; this.listeners = new Map(); this.closed = false; }
    addEventListener(type, callback) { this.listeners.set(type, callback); }
    close() { this.closed = true; }
    emit(type, value) { this.listeners.get(type)?.({ data: JSON.stringify(value) }); }
  }
  const window = { location: { hash: '', pathname: '/', search: '' }, addEventListener(type, callback) { lifecycle.set(type, callback); } };
  const localStorage = { getItem: (key) => storage.get(key) || null,
    setItem: (key, value) => storage.set(key, value), removeItem: (key) => storage.delete(key) };
  const sandbox = { window, document, localStorage, EventSource, URLSearchParams, Headers, FormData,
    URL: { revokeObjectURL(url) { revoked.push(url); } }, console,
    setTimeout(callback) { const id = ++timerID; timers.set(id, callback); return id; },
    clearTimeout(id) { timers.delete(id); }, requestAnimationFrame() {},
    notices, renders };
  const source = fs.readFileSync('internal/server/assets/app.js', 'utf8');
  const hook = `
    globalThis.room = {state, sendMessage, loadSnapshot, connectEvents, applyEvent,
      updateComposerAvailability, removePendingAttachment, clearReply, setPermissions,
      saveSettings, renderSettings, editSettings, participantAction, retryMessage, cancelMessage,
      refreshDiff, readGitStatus: refreshGitStatus, processingTransitionAllowed,
      initializeRoomLocalState, persistComposerDraft, scheduleReconnect, loadOlderMessages,
      setAPI(callback) { api = callback; }};
    render = (force) => renders.push(force);
    toast = (message) => notices.push(message);
    renderAttachmentStrip = refreshGitStatus = postSurfaceState = autoSizeComposer =
      renderMessage = renderParticipants = renderTimeline = queueRender = scrollBottom = setConnection = updateDeliveryHint = updateNotificationButton = recomputeUnread = () => {};
  `;
  assert.ok(source.endsWith('  bootRoom();\n})();\n'), 'keep unit-test boot interception explicit');
  vm.runInNewContext(source.replace(/  bootRoom\(\);\n\}\)\(\);\n$/, hook + '\n})();\n'), sandbox);
  const input = nodes.get('message-input');
  function edit(text) { input.value = text; input.listeners.get('input')(); }
  function key(extra = {}) {
    const event = { key: 'Enter', preventDefault() { this.prevented = true; }, ...extra };
    input.listeners.get('keydown')(event);
    return event;
  }
  nodes.get('message-intent').value = 'steer';
  const snapshot = () => ({ meta: { id: 'test', name: 'Test' }, latest_seq: 10, settings: {stall_warning_seconds:300}, messages: [], participants: {}, approvals: [], events: [] });
  sandbox.room.state.snapshot = snapshot();
  return { ...sandbox.room, nodes, input, edit, key, timers, storage, localStorage, notices, renders, snapshot, lifecycle, revoked };
}

async function main() {
  {
    const c = client();
    c.state.settingsDirty = true;
    c.state.attachmentObjectURLs.add('blob:pending');
    c.state.mediaObjectURLs.set('image', 'blob:history');
    c.connectEvents();
    const stream = c.state.source;
    let prompted = false;
    c.lifecycle.get('beforeunload')({ preventDefault() { prompted = true; } });
    assert.equal(prompted, true, 'unsaved settings still prompt before leaving');
    assert.equal(c.revoked.length, 0, 'cancelling navigation must keep image previews usable');
    assert.equal(stream.closed, false, 'cancelling navigation must not silently kill live updates');
    c.lifecycle.get('pagehide')({ persisted: true });
    assert.equal(c.revoked.length, 0, 'back-forward cache retains its image URLs');
    assert.equal(stream.closed, true, 'cached pages must stop retaining a Room runtime');
    c.setAPI(async () => c.snapshot());
    c.lifecycle.get('pageshow')({ persisted: true });
    await c.state.snapshotPromise;
    assert.notEqual(c.state.source, stream, 'cache restoration reads a fresh snapshot and reconnects');
    c.lifecycle.get('pagehide')({ persisted: false });
    assert.deepEqual(c.revoked, ['blob:pending', 'blob:history']);
    assert.equal(stream.closed, true, 'real page destruction releases the stream');
  }
  {
    const c = client(), request = deferred(), sent = [];
    c.setAPI((_path, options) => { sent.push(JSON.parse(options.body)); return request.promise; });
    c.edit('中文输入确认');
    for (const extra of [{ isComposing: true }, { keyCode: 229 }, { repeat: true }, { shiftKey: true }, { altKey: true }]) {
      assert.equal(c.key(extra).prevented, undefined, 'IME/newline/repeat must not submit');
    }
    c.input.listeners.get('compositionstart')();
    c.key();
    assert.equal(sent.length, 0);
    c.input.listeners.get('compositioncancel')();
    c.key();
    assert.equal(sent.length, 1, 'cancelled IME must not pin the composer closed');
    request.resolve({});
    await c.sendMessage();
    await Promise.resolve();
    assert.equal(c.state.sending, false);
  }
  {
    const c = client(), request = deferred(), sent = [];
    c.setAPI((_path, options) => { sent.push(JSON.parse(options.body)); return request.promise; });
    c.edit('click during IME');
    c.input.listeners.get('compositionstart')();
    c.nodes.get('send-button').listeners.get('click')();
    assert.equal(sent.length, 1, 'explicit Send commits IME and submits');
    request.resolve({});
    await c.sendMessage();
    await Promise.resolve();
    assert.equal(c.state.sending, false);
  }
  {
    const c = client(), request = deferred(), sent = [];
    c.setAPI((_path, options) => { sent.push(JSON.parse(options.body)); return request.promise; });
    c.edit('中文输入确认');
    c.input.listeners.get('compositionstart')();
    c.key();
    assert.equal(sent.length, 0);
    c.input.listeners.get('compositionend')();
    c.key(); c.key();
    c.updateComposerAvailability(); // A render/upload completion cannot unlock a request.
    assert.equal(sent.length, 1, 'rapid Enter must not create duplicate native Turns');
    assert.equal(c.nodes.get('send-button').disabled, true);
    c.edit('next draft');
    request.resolve({});
    await c.sendMessage(); // Already in flight: no second submission.
    await Promise.resolve();
    assert.equal(c.input.value, 'next draft', 'in-flight edits survive acceptance');
    assert.equal(c.state.sending, false);
    assert.equal(c.nodes.get('send-button').disabled, false);
  }
  {
    const c = client(), request = deferred();
    c.setAPI(() => request.promise);
    c.edit('original');
    const sending = c.sendMessage();
    c.edit('changed'); c.edit('original');
    request.resolve({}); await sending;
    assert.equal(c.input.value, 'original', 'revision protects edit-and-revert, not just text equality');
  }
  {
    const c = client(), request = deferred();
    const old = { key: 'old', status: 'ready', attachment: { id: 'old-id' } };
    c.state.pendingAttachments = [old]; c.state.replyTo = 'old-reply';
    c.setAPI(() => request.promise);
    const sending = c.sendMessage();
    await c.removePendingAttachment('old');
    assert.equal(c.state.pendingAttachments.length, 1, 'in-flight media cannot be deleted');
    c.state.pendingAttachments.push({ key: 'new', status: 'ready', attachment: { id: 'new-id' } });
    c.state.replyRevision++; c.state.replyTo = 'new-reply';
    request.resolve({}); await sending;
    assert.equal(c.state.pendingAttachments.length, 1);
    assert.equal(c.state.pendingAttachments[0].key, 'new', 'new media stays in the next draft');
    assert.equal(c.state.replyTo, 'new-reply');
    assert.equal(old.submitting, false);
  }
  {
    const c = client(); let attempts = 0;
    c.setAPI(async () => { attempts++; throw new Error('disconnected'); });
    c.edit('keep this draft'); await c.sendMessage();
    assert.equal(c.input.value, 'keep this draft');
    assert.equal(c.state.sending, false);
    assert.equal(attempts, 1, 'never automatically retry mutations');
    assert.equal(c.timers.size, 0);
    c.setAPI(async () => ({})); await c.sendMessage();
    assert.equal(c.input.value, '', 'unchanged accepted draft is cleared');
  }
  {
    const c = client(), request = deferred(); let reads = 0;
    c.setAPI(() => { reads++; return request.promise; });
    c.connectEvents(); const obsolete = c.state.source;
    c.edit('draft survives resync');
    c.state.localRoomID = 'test';
    c.state.drafts.claude = 'stale transient';
    const first = c.loadSnapshot(), second = c.loadSnapshot();
    assert.equal(first, second, 'snapshot reads must be single-flight');
    for (let i = 0; i < 40; i++) obsolete.emit('pairroom', { seq: 100 + i });
    obsolete.emit('error');
    assert.equal(reads, 1, 'stale buffered events cannot start resnapshot storms');
    assert.equal(obsolete.closed, true);
    request.resolve(c.snapshot()); await first;
    assert.equal(c.input.value, 'draft survives resync');
    assert.equal(c.state.drafts.claude, '');
    assert.equal(c.renders.at(-1), false, 'reconnect must not force a scroll to the bottom');
    assert.notEqual(c.state.source, obsolete);
    assert.equal(c.state.snapshotPromise, null);
  }
  {
    const c = client(); let reads = 0;
    c.setAPI(async () => { reads++; if (reads < 2) throw new Error('offline'); return c.snapshot(); });
    c.connectEvents(); c.state.source.emit('error');
    c.scheduleReconnect();
    assert.equal(c.timers.size, 1, 'only one bounded-backoff timer');
    const tick = async () => {
      const [id, callback] = c.timers.entries().next().value;
      c.timers.delete(id); callback();
      await c.state.snapshotPromise?.catch(() => {});
    };
    await tick();
    assert.equal(c.timers.size, 1, 'failed read schedules recovery');
    assert.equal(c.state.snapshot.latest_seq, 10, 'failed read retains the visible projection');
    await tick();
    assert.equal(c.timers.size, 0);
    assert.equal(reads, 2);
    assert.ok(c.state.source);
  }
  {
    const c = client();
    for (const method of ['getItem', 'setItem', 'removeItem']) c.localStorage[method] = () => { throw new Error('SecurityError'); };
    assert.doesNotThrow(() => c.initializeRoomLocalState());
    c.edit('in-memory draft');
    assert.doesNotThrow(() => c.persistComposerDraft());
    c.initializeRoomLocalState();
    assert.equal(c.input.value, 'in-memory draft');
  }
  {
    const c = client(), request = deferred();
    c.state.snapshot.messages = [{id: 'old', seq: 8}];
    c.state.snapshot.message_window = {has_more: true, total: 8};
    c.setAPI(() => request.promise);
    const loading = c.loadOlderMessages();
    c.state.snapshot = c.snapshot();
    request.resolve({messages: [{id: 'obsolete', seq: 4}], total: 4});
    await loading;
    assert.equal(c.state.snapshot.messages.length, 0, 'stale history response cannot overwrite resynchronized state');
  }
  {
    const c = client(), oldRead = deferred(), write = deferred(); let reads = 0, writes = 0;
    c.setAPI((_path, options) => {
      if (options?.method === 'PUT') { writes++; return write.promise; }
      reads++;
      if (reads === 1) return oldRead.promise;
      const fresh = c.snapshot(); fresh.participants.codex = {permission_profile:'read-only'}; return Promise.resolve(fresh);
    });
    const pending = c.loadSnapshot();
    const change = c.setPermissions('codex','read-only');
    await c.setPermissions('codex','yolo');
    assert.equal(writes, 1, 'permission writes cannot overlap even with a rebuilt DOM');
    write.resolve({ok:true}); await Promise.resolve();
    assert.equal(reads, 1, 'wait for the obsolete read before requesting fresh state');
    oldRead.resolve(c.snapshot()); await pending; await change;
    assert.equal(reads, 2, 'a pre-write snapshot cannot confirm permission changes');
    assert.equal(c.state.snapshot.participants.codex.permission_profile, 'read-only');
    assert.equal(c.state.permissionSubmitting.size, 0);
  }
  {
    const c = client(); let writes = 0;
    c.setAPI(async () => { writes++; throw new Error('stop failed'); });
    await c.setPermissions('claude', 'yolo');
    assert.equal(writes, 1, 'failed permission mutation is never automatically retried');
    assert.equal(c.state.permissionSubmitting.size, 0);
    assert.ok(c.notices.includes('stop failed'));
  }
  {
    const c = client();
    c.renderSettings();
    c.nodes.get('stall-warning').value = '600'; c.editSettings();
    c.state.snapshot.settings.stall_warning_seconds = 900; c.renderSettings();
    assert.equal(c.nodes.get('stall-warning').value, '600', 'remote settings must not erase unsaved edits');
    assert.equal(c.state.settingsDirty, true);
    const oldRead = deferred(), write = deferred(); let reads = 0, writes = 0;
    c.setAPI((_path, options) => {
      if (options?.method === 'PUT') { writes++; return write.promise; }
      reads++; if (reads === 1) return oldRead.promise;
      const result = c.snapshot(); result.settings.stall_warning_seconds = 600; return Promise.resolve(result);
    });
    const loading = c.loadSnapshot(), saving = c.saveSettings();
    await c.saveSettings(); assert.equal(writes, 1);
    c.nodes.get('stall-warning').value = '1200'; c.editSettings();
    write.resolve({}); await Promise.resolve(); assert.equal(reads, 1);
    oldRead.resolve(c.snapshot()); await loading; await saving;
    assert.equal(reads, 2, 'save must be confirmed by a post-write read');
    assert.equal(c.nodes.get('stall-warning').value, '1200', 'edits made during Save must survive confirmation');
    assert.equal(c.state.settingsDirty, true);
    c.setAPI(async (_path, options) => {
      const value = c.snapshot(); value.settings.stall_warning_seconds = 1200; return value;
    });
    await c.saveSettings();
    assert.equal(c.state.settingsDirty, false);
    assert.equal(c.nodes.get('stall-warning').value, 1200);
  }
  {
    const c = client(); c.renderSettings();
    c.nodes.get('stall-warning').value = '600'; c.editSettings();
    c.setAPI(async () => { throw new Error('offline'); });
    await c.saveSettings(); assert.equal(c.nodes.get('stall-warning').value, '600');
    assert.equal(c.state.settingsDirty, true); assert.equal(c.state.settingsSaving, false);
  }
  {
    const c = client(), write = deferred(); let writes = 0;
    c.setAPI((_path, options) => {
      if (options?.method) { writes++; return write.promise; }
      return Promise.resolve(c.snapshot());
    });
    const start = c.participantAction('claude', 'restart');
    await c.participantAction('claude', 'stop');
    await c.setPermissions('claude', 'yolo');
    assert.equal(writes, 1, 'participant operations and permissions share one in-flight owner');
    write.resolve({}); await start; assert.equal(c.state.participantActions.size, 0);
  }
  {
    const c = client(), request = deferred(); let writes = 0;
    c.setAPI((_path, options) => {
      if (options?.method === 'POST') { writes++; return request.promise; }
      const snapshot = c.snapshot(); snapshot.messages = [{ id:'child', retry_of:'source', processing:{codex:'waiting'} }];
      return Promise.resolve(snapshot);
    });
    const pending = c.retryMessage('source', 'codex', {});
    await c.retryMessage('source', 'codex', {}); await c.cancelMessage('source', 'codex', {});
    assert.equal(writes, 1, 'DOM replacement cannot unlock a native message operation');
    request.resolve({}); await pending;
    await c.retryMessage('source', 'codex', {});
    assert.equal(writes, 1, 'visible pending retry stays excluded after the HTTP response');
    assert.equal(c.state.messageActions.size, 0);
  }
  {
    const c = client(), request = deferred(); let writes = 0;
    c.setAPI((_path, options) => { if (options?.method) { writes++; return request.promise; } return Promise.resolve(c.snapshot()); });
    const pending = c.cancelMessage('source', 'codex', {}); await c.cancelMessage('source', 'codex', {});
    assert.equal(writes, 1); request.resolve({}); await pending; assert.equal(c.state.messageActions.size, 0);
  }
  {
    const c = client();
    c.state.snapshot.messages = [{id:'message', processing:{codex:'completed'}}];
    c.applyEvent({seq:11,kind:'message.processing.updated',data:{message_id:'message',target:'codex',state:'working'}});
    assert.equal(c.state.snapshot.messages[0].processing.codex, 'completed', 'late receipts cannot revive settled work');
    assert.equal(c.processingTransitionAllowed('working','waiting'),false);
    assert.equal(c.processingTransitionAllowed('working','failed'),true);
    c.state.snapshot.events = [{id:'evidence',kind:'runtime.event',data:{kind:'log'}}];
    for(let i=0;i<700;i++) c.applyEvent({kind:'runtime.event',data:{agent:'claude',kind:'text.delta',text:'a'}});
    assert.equal(c.state.snapshot.events.length, 1, 'text tokens must not displace diagnostics');
    assert.equal(c.state.drafts.claude.length, 700, 'full text stays in its draft projection');
  }
  {
    const c = client(), older = deferred(), newer = deferred(); let reads = 0;
    c.setAPI(() => (++reads === 1 ? older.promise : newer.promise));
    c.nodes.get('staged-diff').checked = false;
    const first = c.refreshDiff(); c.nodes.get('staged-diff').checked = true; const second = c.refreshDiff();
    newer.resolve({text:'staged evidence'}); await second; older.resolve({text:'obsolete worktree'}); await first;
    assert.equal(c.nodes.get('diff-output').textContent, 'staged evidence');
    assert.equal(c.nodes.get('diff-output').attributes['aria-busy'], 'false');
  }
  {
    const c = client(), older = deferred(); let reads = 0;
    c.setAPI(() => (++reads === 1 ? older.promise : Promise.resolve({text:'latest status'})));
    const first = c.readGitStatus(); await c.readGitStatus(); older.reject(new Error('obsolete error')); await first;
    assert.equal(c.nodes.get('git-status').textContent, 'latest status');
  }
  console.log('room-client transport, lifecycle, settings and Git state: ok');
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
