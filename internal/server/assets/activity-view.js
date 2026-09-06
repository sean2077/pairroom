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
        summary.duration_millis ? h.formatDuration(summary.duration_millis) : '',
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
          entry.items.set(key, row);
        }
        row.className = `turn-item item-${item.kind || 'event'} status-${item.status || 'unknown'}`;
        const heading = row.firstElementChild;
        text(heading.children[0], item.kind || 'event');
        text(heading.children[1], item.name || h.truncate(item.detail, 160) || item.id);
        text(heading.children[2], h.turnStatusText(item.status));
        // Show the full bounded inspector evidence, not only a truncated label.
        text(row.lastElementChild, [item.detail, item.data ? h.prettyJSON(item.data) : ''].filter(Boolean).join('\n\n') || item.id);
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
