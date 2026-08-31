'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

class NativeEventSource {
  constructor() {
    this.listeners = new Map();
    this.nativeClosed = false;
  }

  addEventListener(type, listener) {
    const values = this.listeners.get(type) || [];
    if (!values.includes(listener)) values.push(listener);
    this.listeners.set(type, values);
  }

  removeEventListener(type, listener) {
    const values = this.listeners.get(type) || [];
    this.listeners.set(type, values.filter((value) => value !== listener));
  }

  emit(type, data) {
    for (const listener of [...(this.listeners.get(type) || [])]) {
      if (typeof listener === 'function') listener.call(this, { data });
      else listener.handleEvent({ data });
    }
  }

  close() {
    this.nativeClosed = true;
  }
}

const timers = new Map();
let nextTimer = 1;
const animationFrames = [];
const leaveButton = { listener: null, addEventListener(_type, listener) { this.listener = listener; } };
function turnCard(title, open, sectionOpen) {
  const card = {
    className: 'turn-card turn-claude status-working',
    open,
    firstElementChild: { tagName: 'SUMMARY', textContent: title },
    classList: { contains(value) { return value === 'turn-card'; } },
    querySelector(selector) {
      return selector === '.turn-card-title strong' ? { textContent: title } : null;
    },
    closest(selector) { return selector === 'details.turn-card' ? card : null; },
    sections: [],
  };
  const section = {
    className: 'turn-section',
    open: sectionOpen,
    firstElementChild: { tagName: 'SUMMARY', textContent: 'Plan' },
    classList: { contains() { return false; } },
    querySelector() { return null; },
    closest(selector) { return selector === 'details.turn-card' ? card : null; },
  };
  card.sections.push(section);
  return card;
}

const activity = {
  scrollTop: 17,
  cards: [turnCard('Claude · turn-1', false, true)],
  querySelectorAll(selector) {
    if (selector !== 'details') return [];
    return this.cards.flatMap((card) => [card, ...card.sections]);
  },
};
let historyBacks = 0;
let closes = 0;
let replacedWith = '';

const windowObject = {
  EventSource: NativeEventSource,
  opener: null,
  history: { length: 2, back() { historyBacks += 1; } },
  close() { closes += 1; },
  location: { replace(value) { replacedWith = value; } },
};
const documentObject = {
  hidden: false,
  getElementById(id) {
    if (id === 'leave-room') return leaveButton;
    if (id === 'activity-tab') return activity;
    return null;
  },
};

const context = vm.createContext({
  window: windowObject,
  document: documentObject,
  console,
  JSON,
  Map,
  Set,
  WeakMap,
  Array,
  Number,
  Boolean,
  setTimeout(callback) {
    const id = nextTimer++;
    timers.set(id, callback);
    return id;
  },
  clearTimeout(id) { timers.delete(id); },
  requestAnimationFrame(callback) {
    animationFrames.push(callback);
    return animationFrames.length;
  },
});

const sourceText = fs.readFileSync('internal/server/assets/room-shell.js', 'utf8');
vm.runInContext(sourceText, context, { filename: 'room-shell.js' });
assert.notEqual(windowObject.EventSource, NativeEventSource, 'Room shell must install a scoped EventSource subclass');

function envelope(seq, kind, data = {}) {
  return JSON.stringify({ seq, kind, data });
}

function runTimers() {
  const queued = [...timers.entries()];
  timers.clear();
  queued.forEach(([, callback]) => callback());
}

function runAnimationFrames() {
  while (animationFrames.length) animationFrames.shift()();
}

{
  const source = new windowObject.EventSource('/events');
  const seen = [];
  source.addEventListener('pairroom', (event) => seen.push(JSON.parse(event.data)));
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'text.delta', text: 'a' }));
  assert.equal(seen.length, 1, 'text deltas must remain immediate');
}

{
  const source = new windowObject.EventSource('/events');
  const seen = [];
  source.addEventListener('pairroom', (event) => {
    seen.push(JSON.parse(event.data).data.kind);
    // Simulate app.js replacing the Activity DOM during its scheduled render.
    activity.cards = [turnCard('Claude · turn-1', true, false)];
    activity.scrollTop = 0;
  });
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'command.output', text: 'one' }));
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'usage.updated' }));
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'log', text: 'tool progress' }));
  assert.deepEqual(seen, [], 'high-volume transient events must wait for one batch');
  runTimers();
  runAnimationFrames();
  assert.deepEqual(seen, ['command.output', 'usage.updated', 'log'], 'batching must preserve every transient event and arrival order');
  assert.equal(activity.cards[0].open, false, 'Activity Turn expansion state must survive live rendering');
  assert.equal(activity.cards[0].sections[0].open, true, 'nested Activity sections must survive live rendering');
  assert.equal(activity.scrollTop, 17, 'Activity scroll position must survive live rendering');
}

{
  const source = new windowObject.EventSource('/events');
  const seen = [];
  source.addEventListener('pairroom', (event) => {
    const value = JSON.parse(event.data);
    seen.push(value.seq ? value.kind : value.data.kind);
  });
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'diff.updated' }));
  source.emit('pairroom', envelope(42, 'turn.summary.updated', { id: 'turn-1' }));
  runAnimationFrames();
  assert.deepEqual(seen, ['diff.updated', 'turn.summary.updated'], 'durable events must flush earlier transient telemetry first');
}

{
  const source = new windowObject.EventSource('/events');
  const seen = [];
  source.addEventListener('pairroom', (event) => seen.push(JSON.parse(event.data).data.kind));
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'log', text: 'last progress' }));
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'turn.completed' }));
  runAnimationFrames();
  assert.deepEqual(seen, ['log', 'turn.completed'], 'non-text Runtime boundaries must not overtake queued telemetry');
}

{
  const source = new windowObject.EventSource('/events');
  const seen = [];
  const listener = (event) => seen.push(event.data);
  source.addEventListener('pairroom', listener);
  source.emit('pairroom', envelope(0, 'runtime.event', { kind: 'command.output' }));
  source.close();
  runTimers();
  assert.deepEqual(seen, [], 'closing a stream must discard stale queued presentation events');
  assert.equal(source.nativeClosed, true, 'native EventSource close must still run');
}

assert.equal(typeof leaveButton.listener, 'function', 'Room exit control must be wired');
leaveButton.listener();
assert.equal(historyBacks, 1, 'same-tab Room exit must return to the previous control-plane page');
assert.equal(closes, 0);
assert.equal(replacedWith, '');

console.log('room-shell behavior: ok');
