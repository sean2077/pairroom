(() => {
  'use strict';

  // text.delta already has a dedicated low-cost renderer in app.js. The other
  // transient runtime events can arrive for every command chunk, log/progress
  // line, diff update, or usage sample; delivering them one-by-one makes
  // app.js rebuild the participant cards and Activity inspector continuously.
  // Keep every event, but deliver these presentation-only events in one
  // synchronous batch so the existing requestAnimationFrame scheduler
  // performs a single render.
  const TRANSIENT_RENDER_INTERVAL_MS = 500;
  const BATCHED_RUNTIME_KINDS = new Set([
    'command.output',
    'diff.updated',
    'log',
    'plan.updated',
    'tool.started',
    'tool.completed',
    'usage.updated',
  ]);
  const sourceState = new WeakMap();
  const listenerWrappers = new WeakMap();

  function invokeListener(listener, source, event) {
    if (typeof listener === 'function') {
      listener.call(source, event);
      return;
    }
    if (listener && typeof listener.handleEvent === 'function') listener.handleEvent(event);
  }

  function parseEnvelope(event) {
    try {
      return JSON.parse(event.data);
    } catch (_) {
      return null;
    }
  }

  function isBatchableTransient(envelope) {
    return envelope
      && Number(envelope.seq || 0) === 0
      && envelope.kind === 'runtime.event'
      && BATCHED_RUNTIME_KINDS.has(envelope.data?.kind || '');
  }

  function mayRenderActivity(envelope) {
    if (!envelope) return false;
    if (envelope.kind === 'turn.summary.updated') return true;
    return envelope.kind === 'runtime.event' && envelope.data?.kind !== 'text.delta';
  }

  function detailsStateKey(details, index) {
    const classes = String(details.className || '')
      .split(/\s+/)
      .filter((value) => value && !value.startsWith('status-'))
      .join('.');
    const isTurnCard = details.classList?.contains('turn-card');
    const owner = isTurnCard ? details : details.closest?.('details.turn-card');
    const turn = owner?.querySelector('.turn-card-title strong')?.textContent?.trim() || '';
    const summary = details.firstElementChild?.tagName === 'SUMMARY'
      ? details.firstElementChild.textContent?.trim() || ''
      : '';
    return isTurnCard ? `turn:${turn || index}` : `detail:${turn}|${classes}|${summary || index}`;
  }

  function captureActivityState() {
    const container = document.getElementById('activity-tab');
    if (!container) return null;
    const openDetails = new Map();
    Array.from(container.querySelectorAll('details')).forEach((details, index) => {
      openDetails.set(detailsStateKey(details, index), details.open);
    });
    return { scrollTop: container.scrollTop, openDetails };
  }

  function restoreActivityState(snapshot) {
    if (!snapshot) return;
    const container = document.getElementById('activity-tab');
    if (!container) return;
    Array.from(container.querySelectorAll('details')).forEach((details, index) => {
      const open = snapshot.openDetails.get(detailsStateKey(details, index));
      if (typeof open === 'boolean') details.open = open;
    });
    container.scrollTop = snapshot.scrollTop;
  }

  function scheduleActivityRestore(snapshot) {
    if (!snapshot) return;
    // app.js registers its render callback while processing the event. This
    // callback is registered afterwards, so it restores state after that render.
    requestAnimationFrame(() => restoreActivityState(snapshot));
  }

  function stateFor(source) {
    let state = sourceState.get(source);
    if (!state) {
      state = { timer: 0, records: [] };
      sourceState.set(source, state);
    }
    return state;
  }

  function flushTransientEvents(source) {
    const state = sourceState.get(source);
    if (!state || state.records.length === 0) return;
    if (state.timer) clearTimeout(state.timer);
    state.timer = 0;
    const records = state.records.splice(0);
    const activity = captureActivityState();
    records.forEach((record) => invokeListener(record.listener, source, record.event));
    scheduleActivityRestore(activity);
  }

  function queueTransientEvent(source, listener, event) {
    const state = stateFor(source);
    state.records.push({ listener, event });
    if (state.timer) return;
    state.timer = setTimeout(() => flushTransientEvents(source), TRANSIENT_RENDER_INTERVAL_MS);
  }

  function clearTransientEvents(source) {
    const state = sourceState.get(source);
    if (!state) return;
    if (state.timer) clearTimeout(state.timer);
    state.timer = 0;
    state.records.length = 0;
    sourceState.delete(source);
  }

  function captureOption(options) {
    return typeof options === 'boolean' ? options : Boolean(options?.capture);
  }

  function wrappersFor(source) {
    let wrappers = listenerWrappers.get(source);
    if (!wrappers) {
      wrappers = new Map();
      listenerWrappers.set(source, wrappers);
    }
    return wrappers;
  }

  function wrapperKey(listener, options) {
    return { listener, capture: captureOption(options) };
  }

  function findWrapper(wrappers, listener, options) {
    const capture = captureOption(options);
    for (const [key, wrapper] of wrappers) {
      if (key.listener === listener && key.capture === capture) return { key, wrapper };
    }
    return null;
  }

  const NativeEventSource = window.EventSource;
  if (NativeEventSource) {
    class PairRoomEventSource extends NativeEventSource {
      addEventListener(type, listener, options) {
        if (type !== 'pairroom' || (!listener || (typeof listener !== 'function' && typeof listener.handleEvent !== 'function'))) {
          return super.addEventListener(type, listener, options);
        }
        const wrappers = wrappersFor(this);
        const existing = findWrapper(wrappers, listener, options);
        if (existing) return super.addEventListener(type, existing.wrapper, options);

        const wrapped = (event) => {
          const envelope = parseEnvelope(event);
          if (isBatchableTransient(envelope)) {
            queueTransientEvent(this, listener, event);
            return;
          }
          // A durable event is an ordering boundary. Flush earlier transient
          // telemetry before applying it, while text deltas remain low-latency.
          if (!envelope || Number(envelope.seq || 0) > 0) flushTransientEvents(this);
          const activity = mayRenderActivity(envelope) ? captureActivityState() : null;
          invokeListener(listener, this, event);
          scheduleActivityRestore(activity);
        };
        wrappers.set(wrapperKey(listener, options), wrapped);
        return super.addEventListener(type, wrapped, options);
      }

      removeEventListener(type, listener, options) {
        if (type !== 'pairroom') return super.removeEventListener(type, listener, options);
        const wrappers = listenerWrappers.get(this);
        const existing = wrappers ? findWrapper(wrappers, listener, options) : null;
        if (!existing) return super.removeEventListener(type, listener, options);
        wrappers.delete(existing.key);
        const state = sourceState.get(this);
        if (state) state.records = state.records.filter((record) => record.listener !== listener);
        return super.removeEventListener(type, existing.wrapper, options);
      }

      close() {
        // A snapshot reload replaces all transient presentation state. Do not
        // let queued events from the old stream mutate the new snapshot.
        clearTransientEvents(this);
        listenerWrappers.delete(this);
        return super.close();
      }
    }

    window.EventSource = PairRoomEventSource;
  }

  function leaveRoom() {
    if (window.opener && !window.opener.closed) {
      try {
        window.opener.focus();
      } catch (_) {
        // Cross-window focus is best effort; closing the Room remains valid.
      }
      window.close();
      return;
    }
    if (window.history.length > 1) {
      window.history.back();
      return;
    }
    window.close();
    // Browsers can reject closing a directly opened tab. A blank document is a
    // deterministic final fallback and does not stop either Agent.
    setTimeout(() => {
      if (!document.hidden) window.location.replace('about:blank');
    }, 50);
  }

  document.getElementById('leave-room')?.addEventListener('click', leaveRoom);
})();
