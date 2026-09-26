/* One immutable, unconfirmed publication per Room. No credentials or automatic sends. */
(() => {
  'use strict';
  const MAX_BYTES = 2 * 1024 * 1024;
  const plain = value => value && typeof value === 'object' && !Array.isArray(value);
  const id = value => typeof value === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(value);
  function validReview(a) {
    return plain(a) && Object.keys(a).every(k => ['schema','workspace','base','head','dirty_sha256'].includes(k)) && a.schema === 1 &&
      typeof a.workspace === 'string' && a.workspace.length > 0 && a.workspace.length <= 2048 && !/[\r\n\x00]/.test(a.workspace) &&
      typeof a.base === 'string' && /^[a-f0-9]{40}([a-f0-9]{24})?$/.test(a.base) &&
      typeof a.head === 'string' && /^[a-f0-9]{40}([a-f0-9]{24})?$/.test(a.head) &&
      typeof a.dirty_sha256 === 'string' && /^[a-f0-9]{64}$/.test(a.dirty_sha256);
  }
  function valid(payload) {
    return plain(payload) && Object.keys(payload).every(k => ['id', 'text', 'to', 'attachment_ids', 'review'].includes(k)) &&
      id(payload.id) && typeof payload.text === 'string' && new TextEncoder().encode(payload.text).length <= 256 * 1024 &&
      ['slot1', 'slot2'].includes(payload.to) && Array.isArray(payload.attachment_ids) && payload.attachment_ids.length <= 16 &&
      payload.attachment_ids.every(id) && new Set(payload.attachment_ids).size === payload.attachment_ids.length && (payload.text.length > 0 || payload.attachment_ids.length > 0) &&
      (payload.review === undefined || validReview(payload.review));
  }
  function key(room) {
    if (!id(room)) throw new Error('invalid_room');
    return `pairroom.native.outbox.v1.${room}`;
  }
  function load(storage, room) {
    const raw = storage.getItem(key(room));
    if (raw === null) return null;
    if (raw.length > MAX_BYTES) throw new Error('outbox_invalid');
    let value; try { value = JSON.parse(raw); } catch (_) { throw new Error('outbox_invalid'); }
    if (!plain(value) || value.schema !== 1 || value.room !== room || !valid(value.payload)) throw new Error('outbox_invalid');
    return value.payload;
  }
  // localStorage read/write/verify is not a cross-tab transaction. Mutations
  // use a same-origin Room lock, held only for synchronous storage operations.
  // Never queue an explicit send behind a hidden tab or silently fall back to
  // an unlocked write when Web Locks are unavailable.
  async function mutate(room, operation) {
    const manager = globalThis.navigator?.locks;
    if (!manager || typeof manager.request !== 'function') throw new Error('outbox_unavailable');
    return manager.request(key(room), {mode: 'exclusive', ifAvailable: true}, lock => {
      if (!lock) throw new Error('outbox_conflict');
      return operation();
    });
  }
  async function save(storage, room, payload) {
    if (!valid(payload)) throw new Error('outbox_invalid');
    // Freeze the bytes before awaiting lock acquisition; a caller's mutable
    // object must not change the identity or body reserved by this operation.
    const body = JSON.stringify(payload);
    const raw = JSON.stringify({schema: 1, room, payload});
    if (raw.length > MAX_BYTES) throw new Error('outbox_invalid');
    return mutate(room, () => {
      const old = load(storage, room);
      if (old && JSON.stringify(old) !== body) throw new Error('outbox_conflict');
      storage.setItem(key(room), raw); // Must succeed BEFORE publication.
      if (storage.getItem(key(room)) !== raw) throw new Error('outbox_unavailable');
    });
  }
  async function clear(storage, room, expectedID) {
    return mutate(room, () => {
      const old = load(storage, room);
      if (old && old.id !== expectedID) throw new Error('outbox_conflict');
      storage.removeItem(key(room));
      if (storage.getItem(key(room)) !== null) throw new Error('outbox_unavailable');
    });
  }
  function capture(storage, room) { return storage.getItem(key(room)); }
  async function forget(storage, room, expected) {
    if (expected !== null && typeof expected !== 'string') throw new Error('outbox_conflict');
    return mutate(room, () => {
      // The confirmation dialog may outlive another tab's recovery/new draft.
      // Corrupt records are removable, but only the exact bytes confirmed.
      if (capture(storage, room) !== expected) throw new Error('outbox_conflict');
      storage.removeItem(key(room));
      if (capture(storage, room) !== null) throw new Error('outbox_unavailable');
    });
  }
  function matches(payload, message) {
    return Boolean(message) && message.from === 'user' && message.text === payload.text && message.to === payload.to &&
      JSON.stringify((message.attachments || []).map(a => a.id).sort()) === JSON.stringify([...payload.attachment_ids].sort()) &&
      JSON.stringify(message.review || null) === JSON.stringify(payload.review || null);
  }
  window.PairRoomNativeOutbox = {load, save, clear, capture, forget, matches};
})();
