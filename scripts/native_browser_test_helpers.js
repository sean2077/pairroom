'use strict';
// Deterministic browser boundary doubles, shared between independent VM tabs.
// These exercise the shipped JS; they do not certify an actual browser engine.
function locks() {
  const held = new Set();
  return {
    async request(name, options, callback) {
      if (options.mode !== 'exclusive' || options.ifAvailable !== true) throw Error('unexpected lock policy');
      if (held.has(name)) return callback(null);
      held.add(name);
      try { return await callback({name, mode: 'exclusive'}); }
      finally { held.delete(name); }
    }
  };
}
function timers() {
  const pending = new Map();
  let serial = 0;
  return {
    setTimeout(callback, delay) { const id = ++serial; pending.set(id, {callback, delay}); return id; },
    clearTimeout(id) { pending.delete(id); },
    expire(delay) {
      let count = 0;
      for (const [id, timer] of [...pending]) if (timer.delay === delay) {
        pending.delete(id); timer.callback(); count++;
      }
      return count;
    },
    get size() { return pending.size; }
  };
}
module.exports = {locks, timers};
