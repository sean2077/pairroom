(() => {
  'use strict';

  // Presentation state belongs to these keyed nodes, not to the SSE transport.
  // Complete IDs are keys; translated labels and abbreviated IDs never are.
  function create(container, h) {
    const entries = new Map();
    const transientIDs = new WeakMap();
    let nextTransientID = 0;
    const turnsTitle = element('div', 'activity-section-title');
    const eventsTitle = element('div', 'activity-section-title');
    const empty = element('div', 'activity-empty');

    function element(tag, className, value) {
      const node = document.createElement(tag);
      node.className = className;
      if (value !== undefined) node.textContent = value;
      return node;
    }

    function text(node, value) {
      const content = String(value ?? '');
      if (node.textContent !== content) node.textContent = content;
    }

    function reconcile(parent, nodes) {
      let cursor = parent.firstChild;
      for (const node of nodes) {
        if (node === cursor) cursor = cursor.nextSibling;
        else parent.insertBefore(node, cursor);
      }
      while (cursor) {
        const next = cursor.nextSibling;
        cursor.remove();
        cursor = next;
      }
    }

    function section(key, label, value, parts) {
      let node = parts.get(key);
      if (!node) {
        node = element('details', `turn-section ${key === 'error' ? 'error' : ''}`);
        node.dataset.section = key;
        node.append(element('summary', ''), element('pre', ''));
        parts.set(key, node);
      }
      text(node.firstElementChild, label);
      text(node.lastElementChild, value);
      return node;
    }

    // Tool evidence is loaded on demand from the item's durable source records;
    // older summaries carry bounded evidence inline. Either way nothing is
    // formatted for a collapsed item. Loaded evidence is kept least recently
    // used first within a character budget, and only for an item's current
    // source set; the Service keeps the complete records.
    const evidenceBudget = 2 << 20;
    const evidenceCache = new Map(), evidenceKeys = new Map();
    let evidenceSize = 0;

    function forgetEvidence(key) {
      const cached = evidenceCache.get(key);
      if (!cached) return;
      evidenceCache.delete(key);
      evidenceSize -= cached.text?.length || 0;
      if (evidenceKeys.get(cached.itemKey) === key) evidenceKeys.delete(cached.itemKey);
    }

    function cacheEvidence(key, itemKey, text) {
      forgetEvidence(key);
      evidenceCache.set(key, { itemKey, text });
      evidenceKeys.set(itemKey, key);
      evidenceSize += text.length;
      // The newest entry stays even when it alone exceeds the budget.
      for (const old of evidenceCache.keys()) {
        if (evidenceSize <= evidenceBudget || old === key) break;
        if (evidenceCache.get(old).text !== undefined) forgetEvidence(old);
      }
    }

    function inlineEvidence(item) {
      return [item.detail, item.data ? h.prettyJSON(item.data) : ''].filter(Boolean).join('\n\n') || item.id;
    }

    function formatLoadedEvidence(item, records) {
      if (!records.length) return inlineEvidence(item);
      return records.map((record) => [
        `[${record.kind}]${record.name ? ` ${record.name}` : ''}`,
        record.text || '',
        record.data ? h.prettyJSON(record.data) : '',
        record.truncated ? h.t('room.evidenceTruncated') : '',
      ].filter(Boolean).join('\n')).join('\n\n');
    }

    function showEvidence(row) {
      const state = row._evidence;
      if (!state) return;
      const { summaryID, item } = state;
      const target = row.lastElementChild;
      const sources = item.source_seqs || [];
      if (!sources.length || typeof h.loadTurnItem !== 'function') {
        text(target, inlineEvidence(item));
        return;
      }
      // New source records (for example the completion) invalidate the cache.
      const itemKey = JSON.stringify([summaryID, item.id]);
      const key = JSON.stringify([summaryID, item.id, sources]);
      const previous = evidenceKeys.get(itemKey);
      if (previous !== key) {
        if (previous !== undefined) forgetEvidence(previous);
        evidenceKeys.set(itemKey, key);
      }
      let cached = evidenceCache.get(key);
      if (cached?.text !== undefined) {
        // Refresh recency.
        evidenceCache.delete(key);
        evidenceCache.set(key, cached);
        text(target, cached.text);
        return;
      }
      text(target, [item.detail, h.t('room.loadingEvidence')].filter(Boolean).join('\n\n'));
      if (!cached) {
        cached = { itemKey };
        cached.request = Promise.resolve().then(() => h.loadTurnItem(summaryID, item.id)).then((records) => {
          const value = formatLoadedEvidence(item, records || []);
          if (evidenceCache.get(key) === cached) cacheEvidence(key, itemKey, value);
          return value;
        }, (error) => {
          // A failure is shown, not cached, so reopening retries.
          if (evidenceCache.get(key) === cached) forgetEvidence(key);
          return [item.detail, h.t('room.evidenceUnavailable', { value0: error?.message || '' })].filter(Boolean).join('\n\n');
        });
        evidenceCache.set(key, cached);
      }
      // Every row that asks while the load is in flight receives its result,
      // including a row rebuilt after the one that started it was removed.
      cached.request.then((value) => {
        const current = row._evidence;
        if (!row.isConnected || !row.open || !current || JSON.stringify([current.summaryID, current.item.id, current.item.source_seqs || []]) !== key) return;
        text(target, value);
      });
    }

    function updateTurn(summary, entry, scoped) {
      let card = entry.node;
      if (!card) {
        card = entry.node = element('details', 'turn-card');
        card.dataset.turnId = summary.id || summary.turn_id;
        card.open = ['working', 'waiting'].includes(summary.status) || scoped;
        const head = element('summary', 'turn-card-head');
        const title = element('div', 'turn-card-title');
        title.append(element('span', 'turn-status'), element('strong', ''));
        head.append(title, element('span', 'turn-card-meta'));
        card.append(head, element('div', 'turn-card-body'));
        entry.parts = new Map();
        entry.items = new Map();
        entry.list = element('div', 'turn-item-list');
      }
      card.className = `turn-card turn-${summary.agent} status-${summary.status || 'unknown'}`;
      const head = card.firstElementChild, title = head.firstElementChild;
      title.firstElementChild.className = `turn-status status-${summary.status || 'unknown'}`;
      text(title.firstElementChild, h.turnStatusText(summary.status));
      const fullTitle = `${h.displayName(summary.agent)} · ${summary.turn_id || summary.id}`;
      text(title.lastElementChild, `${h.displayName(summary.agent)} · ${h.truncate(summary.turn_id || summary.id, 20)}`);
      title.lastElementChild.title = fullTitle;
      text(head.lastElementChild, [
        summary.duration_millis ? h.formatDurationMillis(summary.duration_millis) : '',
        h.t('room.itemCount', { count: (summary.items || []).length }),
        h.formatTime(summary.updated_at || summary.started_at),
      ].filter(Boolean).join(' · '));

      const body = [];
      for (const [key, label, value] of [
        ['error', 'common.error', summary.error], ['plan', 'common.plan', summary.plan],
        ['diff', 'common.diff', summary.diff], ['final', 'common.final', summary.final_text],
      ]) {
        if (value) body.push(section(key, h.t(label), value, entry.parts));
      }
      const items = [], keys = new Set();
      (summary.items || []).slice(-40).forEach((item, index) => {
        const key = item.id || `missing-id:${index}`;
        keys.add(key);
        let row = entry.items.get(key);
        if (!row) {
          row = element('details', 'turn-item');
          row.dataset.itemId = key;
          const heading = element('summary', 'turn-item-head');
          heading.append(element('span', 'turn-item-tag'), element('span', 'turn-item-text'), element('span', 'turn-item-status'));
          row.append(heading, element('pre', 'turn-item-evidence'));
          // Evidence renders only while open; toggling open renders it once.
          row.addEventListener('toggle', () => { if (row.open) showEvidence(row); });
          entry.items.set(key, row);
        }
        row.className = `turn-item item-${item.kind || 'event'} status-${item.status || 'unknown'}`;
        const heading = row.firstElementChild;
        text(heading.children[0], item.kind || 'event');
        text(heading.children[1], item.name || h.truncate(item.detail, 160) || item.id);
        text(heading.children[2], h.turnStatusText(item.status));
        row._evidence = { summaryID: summary.id, item };
        if (row.open) showEvidence(row);
        items.push(row);
      });
      for (const key of entry.items.keys()) if (!keys.has(key)) entry.items.delete(key);
      reconcile(entry.list, items);
      if (items.length) body.push(entry.list);
      if (summary.usage) body.push(section('usage', h.t('common.usage'), h.prettyJSON(summary.usage), entry.parts));
      reconcile(card.lastElementChild, body);
      return card;
    }

    function eventKey(event) {
      if (event.id) return `event:${event.id}`;
      if (event.seq) return `seq:${event.seq}`;
      if (!transientIDs.has(event)) transientIDs.set(event, ++nextTransientID);
      return `transient:${transientIDs.get(event)}`;
    }

    function updateEvent(envelope, entry) {
      const event = envelope.data || {};
      let card = entry.node;
      if (!card) {
        card = entry.node = element('div', 'activity-card');
        const head = element('div', 'activity-card-head');
        const kind = element('div', 'activity-kind');
        kind.append(element('span', 'activity-icon'), element('span', ''));
        head.append(kind, element('span', 'activity-agent'));
        card.append(head, element('div', 'activity-body'));
      }
      const head = card.firstElementChild;
      text(head.firstElementChild.firstElementChild, h.activityIcon(event.kind));
      text(head.firstElementChild.lastElementChild, h.activityLabel(event));
      text(head.lastElementChild, h.displayName(event.agent));
      text(card.lastElementChild, h.activityDetail(event));
      card.lastElementChild.hidden = !card.lastElementChild.textContent;
      return card;
    }

    function render(summaries, events, options) {
      const focused = container.contains(document.activeElement) ? document.activeElement : null;
      const selection = window.getSelection();
      const savedSelection = selection && !selection.isCollapsed && container.contains(selection.anchorNode)
        ? { anchor: selection.anchorNode, anchorOffset: selection.anchorOffset, focus: selection.focusNode, focusOffset: selection.focusOffset } : null;
      const scrollTop = container.scrollTop;
      const top = container.getBoundingClientRect().top;
      const anchor = scrollTop > 1 ? Array.from(container.children).find((node) => node.dataset.activityKey && node.getBoundingClientRect().bottom > top) : null;
      const offset = anchor ? anchor.getBoundingClientRect().top - top : 0;
      const nodes = [], keep = new Set();
      function item(key, value, update) {
        keep.add(key);
        const entry = entries.get(key) || {};
        // Unchanged event/summary objects do not even serialize their evidence.
        if (entry.value !== value || entry.version !== options.version) {
          const signature = JSON.stringify([options.version, value]);
          if (entry.signature !== signature) update(value, entry, options.scoped);
          entry.signature = signature;
          entry.value = value;
          entry.version = options.version;
        }
        entry.node.dataset.activityKey = key;
        entries.set(key, entry);
        nodes.push(entry.node);
      }
      text(turnsTitle, h.t('room.turnSummaries'));
      text(eventsTitle, h.t('room.recentNativeEvents'));
      if (summaries.length) nodes.push(turnsTitle);
      for (const summary of summaries) item(`turn:${summary.id || JSON.stringify([summary.agent, summary.turn_id])}`, summary, updateTurn);
      if (events.length) nodes.push(eventsTitle);
      for (const event of events) item(eventKey(event), event, updateEvent);
      if (!nodes.length) { text(empty, options.emptyText); nodes.push(empty); }
      reconcile(container, nodes);
      for (const key of entries.keys()) if (!keep.has(key)) entries.delete(key);
      // insertBefore can blur a moved (but not replaced) node. Restore only live
      // endpoints; changed/deleted evidence must not select unrelated new text.
      if (focused?.isConnected && document.activeElement !== focused) focused.focus({ preventScroll: true });
      if (savedSelection?.anchor.isConnected && savedSelection.focus?.isConnected) {
        try { selection.setBaseAndExtent(savedSelection.anchor, savedSelection.anchorOffset, savedSelection.focus, savedSelection.focusOffset); }
        catch (_) { /* A changed text endpoint is no longer a valid selection. */ }
      }
      if (anchor?.isConnected) container.scrollTop += anchor.getBoundingClientRect().top - container.getBoundingClientRect().top - offset;
      else container.scrollTop = scrollTop;
    }
    return { render };
  }
  window.PairRoomActivity = Object.freeze({ create });
})();
