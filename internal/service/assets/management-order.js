(() => {
  'use strict';
  window.PairRoomOrder = { create({ t, node, getSnapshot, api, refresh, render, toast, isCurrent }) {
    let gesture = null, menu = null, saving = false, suppressClick = false, scrollFrame = 0;

    function ordered(items, saved = []) {
      const rank = new Map(saved.map((id, index) => [id, index]));
      return [...items].sort((a, b) => (rank.get(a.id) ?? saved.length) - (rank.get(b.id) ?? saved.length));
    }
    function announce(message) {
      let status = document.getElementById('navigation-order-status');
      if (!status) {
        status = node('div', { id: 'navigation-order-status', className: 'visually-hidden', role: 'status', 'aria-live': 'polite' });
        document.body.append(status);
      }
      status.textContent = message;
    }
    function items(kind, id) {
      const snapshot = getSnapshot();
      if (kind === 'project') return ordered(snapshot.projects || [], snapshot.navigation_order?.projects);
      const room = snapshot.rooms?.find((item) => item.id === id);
      return ordered((snapshot.rooms || []).filter((item) => item.project_id === room?.project_id && item.lifecycle === room?.lifecycle), snapshot.navigation_order?.rooms?.[room?.project_id]);
    }
    const FOCUSABLE = 'a[href],button:not([disabled]),input,select,textarea,[tabindex]:not([tabindex="-1"])';
    function rowSelector(kind, id) {
      return `[data-order-kind="${kind}"][data-order-id="${CSS.escape(id)}"]`;
    }
    async function move(kind, id, targetID, position, scope) {
      if (saving || !targetID || id === targetID) return;
      closeMenu();
      saving = true;
      const current = isCurrent();
      announce(t('workspace.ordering.saving'));
      document.querySelectorAll('[data-order-kind]').forEach((row) => { row.classList.add('order-saving'); });
      try {
        await api('/api/v1/navigation-order', { method: 'PATCH', body: JSON.stringify({ kind, id, target_id: targetID, position }) });
        if (!current()) return;
        // Discard reads started before the mutation. The Service is the sole
        // owner of order; a failed save never leaves optimistic UI ranks.
        await refresh({ fresh: true });
        if (current()) announce(t('workspace.ordering.saved'));
      } catch (error) {
        if (current() && error.status !== 401) {
          toast(t('workspace.ordering.failed'), t('workspace.ordering.retry'), 'error');
          announce(t('workspace.ordering.failed'));
          await refresh({ fresh: true });
        }
      } finally {
        saving = false;
        if (current()) {
          document.querySelectorAll('[data-order-kind]').forEach((row) => { row.classList.remove('order-saving'); });
          // render() rebuilds the row, so remember which of its controls held
          // focus and restore it; Alt+Arrow must work repeatedly.
          const before = document.querySelector(`${scope} ${rowSelector(kind, id)}`);
          const controls = before ? [...before.querySelectorAll(FOCUSABLE)] : [];
          const held = controls.indexOf(document.activeElement);
          const restore = held >= 0 ? held : document.activeElement === document.body ? 0 : -1;
          render();
          if (restore >= 0) {
            const after = document.querySelector(`${scope} ${rowSelector(kind, id)}`);
            const next = after ? [...after.querySelectorAll(FOCUSABLE)] : [];
            next[Math.min(restore, next.length - 1)]?.focus({ preventScroll: true });
          }
        }
      }
    }
    function step(kind, id, delta, scope) {
      const list = items(kind, id);
      const target = list[list.findIndex((item) => item.id === id) + delta];
      if (target) void move(kind, id, target.id, delta < 0 ? 'before' : 'after', scope);
    }
    function closeMenu() {
      if (!menu) return;
      menu.row.classList.remove('order-menu-open');
      menu.element.remove();
      menu = null;
    }
    function openMenu(row, kind, id, scope) {
      if (menu?.row === row) { closeMenu(); return; }
      closeMenu();
      const list = items(kind, id), index = list.findIndex((item) => item.id === id);
      const element = node('div', { className: 'order-menu', role: 'group', 'aria-label': t('workspace.ordering.actions') },
        node('button', { type: 'button', textContent: t('workspace.ordering.up'), disabled: index <= 0, onClick: () => step(kind, id, -1, scope) }),
        node('button', { type: 'button', textContent: t('workspace.ordering.down'), disabled: index < 0 || index === list.length - 1, onClick: () => step(kind, id, 1, scope) })
      );
      row.append(element);
      row.classList.add('order-menu-open');
      menu = { element, row };
      const boundary = row.closest('#room-tree')?.getBoundingClientRect().bottom || innerHeight;
      if (element.getBoundingClientRect().bottom > Math.min(innerHeight, boundary)) element.classList.add('order-menu-above');
      element.querySelector('button:not(:disabled)')?.focus({ preventScroll: true });
    }
    function clearTarget() {
      if (!gesture?.target) return;
      gesture.target.classList.remove('order-before', 'order-after');
      gesture.target = null;
    }
    function targetAtPointer() {
      if (!gesture?.dragging) return;
      clearTarget();
      const target = document.elementFromPoint(gesture.x, gesture.y)?.closest(`[data-order-kind="${gesture.kind}"]`);
      if (!target || target === gesture.row || !target.closest(gesture.scope) || target.dataset.orderGroup !== gesture.row.dataset.orderGroup) return;
      const rect = target.getBoundingClientRect();
      gesture.position = gesture.y < rect.top + rect.height / 2 ? 'before' : 'after';
      gesture.target = target;
      target.classList.add(`order-${gesture.position}`);
    }
    function autoScroll() {
      if (!gesture?.dragging) return;
      const scroll = gesture.scroll;
      const rect = scroll === document.scrollingElement ? { top: 0, bottom: innerHeight } : scroll.getBoundingClientRect();
      const top = Math.max(0, rect.top), bottom = Math.min(innerHeight, rect.bottom);
      const delta = gesture.y < top + 36 ? -10 : gesture.y > bottom - 36 ? 10 : 0;
      if (delta) { scroll.scrollTop += delta; targetAtPointer(); }
      scrollFrame = requestAnimationFrame(autoScroll);
    }
    function stopGesture() {
      if (!gesture) return null;
      const old = gesture;
      clearTarget();
      gesture = null;
      cancelAnimationFrame(scrollFrame);
      old.row.classList.remove('order-dragging', 'order-holding');
      document.body.classList.remove('navigation-dragging');
      if (old.row.hasPointerCapture(old.pointerID)) old.row.releasePointerCapture(old.pointerID);
      return old;
    }
    function cancel() {
      const old = stopGesture();
      if (old?.dragging) announce(t('workspace.ordering.cancelled'));
      closeMenu();
    }
    function startDragging(gesture) {
      if (gesture.dragging) return;
      gesture.dragging = true;
      gesture.row.classList.remove('order-holding');
      gesture.row.classList.add('order-dragging');
      document.body.classList.add('navigation-dragging');
      // Capture only once the gesture commits: capturing at pointerdown would
      // retarget the press/release sequence to the row and eat child clicks.
      try { gesture.row.setPointerCapture(gesture.pointerID); } catch { /* pointer already released */ }
      announce(t('workspace.ordering.help'));
      autoScroll();
    }
    // Drag affordance is the whole row. Interactive children stay clickable
    // because only a 6px move commits the gesture; a press that never moves is
    // a plain click, however long it is held.
    function decorate(row, kind, id) {
      const snapshot = getSnapshot();
      if (!snapshot?.navigation_order) return row;
      const item = (kind === 'project' ? snapshot.projects : snapshot.rooms)?.find((value) => value.id === id);
      if (!item) return row;
      const sortable = !(snapshot.navigation_order_error || items(kind, id).length < 2);
      row.dataset.orderKind = kind;
      row.dataset.orderId = id;
      row.dataset.orderGroup = kind === 'project' ? 'projects' : `${item.project_id}/${item.lifecycle}`;
      if (saving) row.classList.add('order-saving');
      if (!sortable) return row;
      row.title = t('workspace.ordering.help');
      const scope = () => row.closest('#room-tree') ? '#room-tree' : '#view';
      row.addEventListener('contextmenu', (event) => {
        if (saving) return;
        event.preventDefault(); event.stopPropagation();
        openMenu(row, kind, id, scope());
      });
      // Keyboard parity without an extra tab stop: Alt+Arrow moves the row that
      // currently holds focus, matching the right-click move menu.
      row.addEventListener('keydown', (event) => {
        if (saving || !event.altKey || (event.key !== 'ArrowUp' && event.key !== 'ArrowDown')) return;
        if (!row.contains(document.activeElement)) return;
        event.preventDefault(); event.stopPropagation();
        step(kind, id, event.key === 'ArrowUp' ? -1 : 1, scope());
      });
      row.addEventListener('pointerdown', (event) => {
        // A press inside the open move menu belongs to the menu: starting (or
        // cancelling) a gesture here would detach the button before its click.
        if (menu?.element.contains(event.target)) return;
        // A second press while a gesture is held cancels it (native text
        // selection or an out-of-row release both look like this on a row).
        if (gesture?.pointerID !== event.pointerId) cancel();
        // Touch keeps native list scrolling; a long press opens the move menu.
        if (saving || gesture || event.button !== 0 || event.isPrimary === false || event.pointerType === 'touch') return;
        closeMenu(); suppressClick = false;
        let scroll = row.parentElement;
        while (scroll && !/(auto|scroll)/.test(getComputedStyle(scroll).overflowY)) scroll = scroll.parentElement;
        gesture = { row, kind, id, scope: scope(), pointerID: event.pointerId, startX: event.clientX, startY: event.clientY, x: event.clientX, y: event.clientY, scroll: scroll || document.scrollingElement, dragging: false, target: null };
        row.classList.add('order-holding');
      });
      return row;
    }
    // Move/release are tracked document-wide: the pointer may leave the row
    // before the 6px threshold, and capture only starts once dragging commits.
    document.addEventListener('pointermove', (event) => {
      if (!gesture || gesture.pointerID !== event.pointerId) return;
      gesture.x = event.clientX; gesture.y = event.clientY;
      if (!gesture.dragging && Math.hypot(gesture.x - gesture.startX, gesture.y - gesture.startY) >= 6) startDragging(gesture);
      if (gesture.dragging) { event.preventDefault(); targetAtPointer(); }
    });
    document.addEventListener('pointerup', (event) => {
      if (!gesture || gesture.pointerID !== event.pointerId) return;
      const { kind, id, scope } = gesture;
      const targetID = gesture.target?.dataset.orderId, position = gesture.position;
      const old = stopGesture();
      if (old.dragging) {
        event.preventDefault(); suppressClick = true;
        if (targetID) void move(kind, id, targetID, position, scope);
        else { announce(t('workspace.ordering.cancelled')); render(); }
      }
    });
    document.addEventListener('click', (event) => {
      if (!suppressClick) return;
      suppressClick = false;
      event.preventDefault(); event.stopPropagation();
    }, true);
    document.addEventListener('pointercancel', cancel);
    document.addEventListener('lostpointercapture', () => { if (gesture && !gesture.row.hasPointerCapture(gesture.pointerID)) cancel(); });
    document.addEventListener('pointerdown', (event) => {
      // Never let a stale drag flag eat an unrelated later click.
      suppressClick = false;
      if (menu && !menu.element.contains(event.target) && !menu.row.contains(event.target)) closeMenu();
    });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && (gesture || menu)) {
        event.preventDefault(); cancel();
      }
    });
    window.addEventListener('blur', cancel);
    document.addEventListener('visibilitychange', () => { if (document.hidden) cancel(); });
    return { ordered, decorate, cancel, interacting: () => Boolean(gesture || menu) };
  } };
})();
