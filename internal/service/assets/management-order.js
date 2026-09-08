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
    function controlSelector(kind, id) {
      return `[data-order-kind="${kind}"][data-order-id="${CSS.escape(id)}"] .order-handle`;
    }
    async function move(kind, id, targetID, position, scope) {
      if (saving || !targetID || id === targetID) return;
      closeMenu();
      saving = true;
      const current = isCurrent();
      announce(t('workspace.ordering.saving'));
      document.querySelectorAll('.order-handle').forEach((button) => { button.disabled = true; });
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
          const focusHere = document.activeElement === document.body || document.activeElement?.classList.contains('order-handle');
          render();
          if (focusHere) document.querySelector(`${scope} ${controlSelector(kind, id)}`)?.focus({ preventScroll: true });
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
      menu.button.setAttribute('aria-expanded', 'false');
      menu.row.classList.remove('order-menu-open');
      menu.element.remove();
      menu = null;
    }
    function openMenu(button, row, kind, id, scope) {
      if (menu?.button === button) { closeMenu(); return; }
      closeMenu();
      const list = items(kind, id), index = list.findIndex((item) => item.id === id);
      const element = node('div', { className: 'order-menu', role: 'group', 'aria-label': t('workspace.ordering.actions') },
        node('button', { type: 'button', textContent: t('workspace.ordering.up'), disabled: index <= 0, onClick: () => step(kind, id, -1, scope) }),
        node('button', { type: 'button', textContent: t('workspace.ordering.down'), disabled: index < 0 || index === list.length - 1, onClick: () => step(kind, id, 1, scope) })
      );
      button.setAttribute('aria-expanded', 'true');
      row.append(element);
      row.classList.add('order-menu-open');
      menu = { button, element, row };
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
      old.row.classList.remove('order-dragging');
      document.body.classList.remove('navigation-dragging');
      if (old.button.hasPointerCapture(old.pointerID)) old.button.releasePointerCapture(old.pointerID);
      return old;
    }
    function cancel() {
      const old = stopGesture();
      if (old?.dragging) announce(t('workspace.ordering.cancelled'));
      closeMenu();
    }
    function decorate(row, kind, id, handleHost = row) {
      const snapshot = getSnapshot();
      if (!snapshot?.navigation_order) return row;
      const item = (kind === 'project' ? snapshot.projects : snapshot.rooms)?.find((value) => value.id === id);
      if (!item) return row;
      row.dataset.orderKind = kind;
      row.dataset.orderId = id;
      row.dataset.orderGroup = kind === 'project' ? 'projects' : `${item.project_id}/${item.lifecycle}`;
      const name = item.name || item.root?.split(/[\\/]/).filter(Boolean).pop() || id;
      const button = node('button', { type: 'button', className: 'order-handle', textContent: '⠿',
        'data-order-key': `${kind}:${id}`, title: t('workspace.ordering.help'), 'aria-label': t('workspace.ordering.label', { name }), 'aria-expanded': 'false',
        disabled: saving || snapshot.navigation_order_error || items(kind, id).length < 2 });
      const scope = () => row.closest('#room-tree') ? '#room-tree' : '#view';
      button.addEventListener('click', (event) => {
        event.stopPropagation();
        if (suppressClick) { suppressClick = false; return; }
        if (!saving) openMenu(button, row, kind, id, scope());
      });
      button.addEventListener('keydown', (event) => {
        if (event.key === 'ArrowUp' || event.key === 'ArrowDown') {
          event.preventDefault(); event.stopPropagation();
          step(kind, id, event.key === 'ArrowUp' ? -1 : 1, scope());
        }
      });
      button.addEventListener('pointerdown', (event) => {
        if (saving || gesture || event.button !== 0 || event.isPrimary === false) return;
        closeMenu(); suppressClick = false;
        let scroll = row.parentElement;
        while (scroll && !/(auto|scroll)/.test(getComputedStyle(scroll).overflowY)) scroll = scroll.parentElement;
        gesture = { row, button, kind, id, scope: scope(), pointerID: event.pointerId, startX: event.clientX, startY: event.clientY, x: event.clientX, y: event.clientY, scroll: scroll || document.scrollingElement, dragging: false, target: null };
        button.setPointerCapture(event.pointerId);
      });
      button.addEventListener('pointermove', (event) => {
        if (!gesture || gesture.pointerID !== event.pointerId) return;
        gesture.x = event.clientX; gesture.y = event.clientY;
        if (!gesture.dragging && Math.hypot(gesture.x - gesture.startX, gesture.y - gesture.startY) >= 6) {
          gesture.dragging = true;
          row.classList.add('order-dragging');
          document.body.classList.add('navigation-dragging');
          announce(t('workspace.ordering.help'));
          autoScroll();
        }
        if (gesture.dragging) { event.preventDefault(); targetAtPointer(); }
      });
      button.addEventListener('pointerup', (event) => {
        if (!gesture || gesture.pointerID !== event.pointerId) return;
        const targetID = gesture.target?.dataset.orderId, position = gesture.position;
        const old = stopGesture();
        if (old.dragging) {
          event.preventDefault(); suppressClick = true;
          if (targetID) void move(kind, id, targetID, position, old.scope);
          else { announce(t('workspace.ordering.cancelled')); render(); }
        }
      });
      button.addEventListener('pointercancel', cancel);
      button.addEventListener('lostpointercapture', () => { if (gesture?.button === button) cancel(); });
      handleHost.prepend(button);
      return row;
    }
    document.addEventListener('pointerdown', (event) => { if (menu && !menu.element.contains(event.target) && menu.button !== event.target) closeMenu(); });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && (gesture || menu)) {
        const button = gesture?.button || menu?.button;
        event.preventDefault(); cancel(); button?.focus({ preventScroll: true });
      }
    });
    window.addEventListener('blur', cancel);
    document.addEventListener('visibilitychange', () => { if (document.hidden) cancel(); });
    return { ordered, decorate, cancel, interacting: () => Boolean(gesture || menu) };
  } };
})();
