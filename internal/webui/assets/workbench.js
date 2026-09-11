/* Dependency-free chrome icons; never observes or renders the conversation. */
(() => {
  'use strict';
  const shapes = {
    overview: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M9 9v12"/>',
    folder: '<path d="M3 7V5a2 2 0 0 1 2-2h5l2 3h7a2 2 0 0 1 2 2v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7Z"/>',
    terminal: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="m7 9 3 3-3 3m6 0h4"/>',
    settings: '<path d="M4 7h6m4 0h6M4 17h10m4 0h2"/><circle cx="12" cy="7" r="2"/><circle cx="16" cy="17" r="2"/>',
    search: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 5 5"/>',
    refresh: '<path d="M20 7v5h-5M4 17v-5h5m10-5a8 8 0 0 0-13-1m-1 11a8 8 0 0 0 13 1"/>',
    bell: '<path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4"/>',
    sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5"/>',
    moon: '<path d="M20 14a8 8 0 0 1-10-10 8.5 8.5 0 1 0 10 10Z"/>',
    system: '<rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8m-4-4v4"/>',
    menu: '<path d="M4 6h16M4 12h16M4 18h16"/>',
    plus: '<path d="M12 4v16M4 12h16"/>',
    close: '<path d="m6 6 12 12M6 18 18 6"/>',
    maximize: '<path d="M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5"/>',
    layout: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M8 4v16m8-16v16"/>',
    command: '<path d="M9 7V5a2 2 0 1 0-2 2h10a2 2 0 1 0-2-2v14a2 2 0 1 0 2-2H7a2 2 0 1 0 2 2V7Z"/>',
  };
  const targets = new Map();
  function paint(node, getName) {
    const name = getName();
    if (node.firstElementChild?.dataset.workbenchIcon === name) return;
    // Only constant project-authored geometry enters this sink, never user text.
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    for (const [key, value] of Object.entries({viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '1.65', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true', focusable: 'false', class: 'workbench-icon'})) svg.setAttribute(key, value);
    svg.dataset.workbenchIcon = name;
    svg.innerHTML = shapes[name];
    node.replaceChildren(svg);
  }
  function compactRoomTools() {
    if (!document.body.classList.contains('workbench-room')) return;
    const footer = document.querySelector('.ux-menu-footer');
    if (!footer) return;
    const row = document.createElement('div');
    row.className = 'workbench-room-utilities';
    footer.prepend(row);
    const tools = ['theme-button', 'refresh-button'].map(id => document.getElementById(id)).filter(Boolean).map(button => {
      const anchor = document.createComment('workbench utility position');
      button.before(anchor);
      return {button, anchor};
    });
    const media = window.matchMedia('(max-width: 720px)');
    function sync() {
      const focused = document.activeElement;
      for (const {button, anchor} of tools) {
        if (media.matches) row.append(button);
        else anchor.after(button);
      }
      row.hidden = !media.matches;
      if (tools.some(({button}) => button === focused)) {
        // Resizing must not strand keyboard focus inside a closed menu.
        const menuClosed = document.getElementById('ux-layout-menu')?.hidden;
        (media.matches && menuClosed ? document.getElementById('ux-layout-button') : focused)?.focus({preventScroll: true});
      }
    }
    media.addEventListener('change', sync);
    sync();
  }
  function start() {
    if (!document.body.classList.contains('workbench')) return;
    compactRoomTools();
    const icons = {
      '[data-nav="overview"] .nav-icon': 'overview',
      '[data-nav="projects"] .nav-icon': 'folder',
      '[data-nav="runtimes"] .nav-icon': 'terminal',
      '[data-nav="settings"] .nav-icon': 'settings',
      '.global-search > span, .search-box > span': 'search',
      '#refresh-button, .room-workspace-refresh': 'refresh',
      '#notification-button': 'bell',
      '#mobile-menu, .room-workspace-menu': 'menu',
      '#room-picker-button, #attach-button': 'plus',
      '.modal-close, .ux-menu-close': 'close',
      '.room-workspace-maximize': 'maximize',
      '#ux-layout-button': 'layout',
      '.management-command-button, .room-workspace-command': 'command',
    };
    for (const [selector, name] of Object.entries(icons)) {
      document.querySelectorAll(selector).forEach(node => targets.set(node, () => name));
    }
    document.querySelectorAll('#theme-button, [data-theme-cycle]').forEach(node => targets.set(node, () => {
      const mode = window.PairRoomTheme?.mode || 'system';
      return mode === 'light' ? 'sun' : mode === 'dark' ? 'moon' : 'system';
    }));
    // Theme/language/notification renderers replace button text. Observe only
    // these fixed chrome nodes (not body/subtree) and ignore our own repaint.
    const observer = new MutationObserver(records => {
      for (const node of new Set(records.map(record => record.target))) {
        const name = targets.get(node);
        if (name) paint(node, name);
      }
    });
    for (const [node, name] of targets) {
      paint(node, name);
      observer.observe(node, {childList: true});
    }
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start, {once: true});
  else start();
})();
