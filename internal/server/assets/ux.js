(() => {
  'use strict';

  const t = (key, options) => window.PairRoomI18n ? window.PairRoomI18n.t(key, options) : key;

  const STORAGE_KEY = 'pairroom.room.ux.v1';
  const MOBILE_QUERY = window.matchMedia('(max-width: 1120px)');
  const DEFAULTS = Object.freeze({
    participantsVisible: true,
    inspectorVisible: true,
    focusMode: false,
    density: 'comfortable',
    participantsWidth: 288,
    inspectorWidth: 380,
  });
  const LIMITS = Object.freeze({
    participants: [220, 420],
    inspector: [280, 560],
  });

  const body = document.body;
  const root = document.documentElement;
  const workspace = document.querySelector('.workspace');
  const topbar = document.querySelector('.topbar');
  const topbarActions = document.querySelector('.topbar-actions');
  const participantsPanel = document.querySelector('.participants-panel');
  const chatPanel = document.querySelector('.chat-panel');
  const inspectorPanel = document.querySelector('.inspector-panel');
  const timeline = document.getElementById('timeline');
  const messageInput = document.getElementById('message-input');
  const messageSearch = document.getElementById('message-search');
  const scrollBottom = document.getElementById('scroll-bottom');

  if (!body || !workspace || !topbar || !topbarActions || !participantsPanel || !chatPanel || !inspectorPanel) return;

  const state = { ...DEFAULTS, ...readStoredState(), mobilePanel: '' };
  let menuOpen = false;
  let lastFocusedElement = null;
  let drawerReturnFocus = null;
  let resizeFrame = 0;

  body.classList.add('ux-enhanced');
  installSkipLink();
  const announcer = installAnnouncer();
  const layoutUI = installLayoutMenu();
  const backdrop = installDrawerBackdrop();
  const leftRail = installPanelRail('participants', t("ui.showParticipantsPanel"), '›');
  const rightRail = installPanelRail('inspector', t("ui.showWorkInspector"), '‹');
  const leftResizer = installResizer('participants');
  const rightResizer = installResizer('inspector');

  improveSemantics();
  installKeyboardNavigation();
  installTimelineAttention();
  installResponsiveListeners();
  applyLayout({ persist: false });

  function readStoredState() {
    try {
      const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}');
      return {
        participantsVisible: stored.participantsVisible !== false,
        inspectorVisible: stored.inspectorVisible !== false,
        focusMode: stored.focusMode === true,
        density: stored.density === 'compact' ? 'compact' : 'comfortable',
        participantsWidth: clamp(Number(stored.participantsWidth) || DEFAULTS.participantsWidth, ...LIMITS.participants),
        inspectorWidth: clamp(Number(stored.inspectorWidth) || DEFAULTS.inspectorWidth, ...LIMITS.inspector),
      };
    } catch {
      return {};
    }
  }

  function storeState() {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify({
        participantsVisible: state.participantsVisible,
        inspectorVisible: state.inspectorVisible,
        focusMode: state.focusMode,
        density: state.density,
        participantsWidth: state.participantsWidth,
        inspectorWidth: state.inspectorWidth,
      }));
    } catch {
      // Layout preferences are an optional enhancement. The room remains fully usable
      // when storage is disabled by the browser or an embedded webview policy.
    }
  }

  function clamp(value, min, max) {
    return Math.min(max, Math.max(min, value));
  }

  function installSkipLink() {
    if (document.querySelector('.ux-skip-link')) return;
    const link = document.createElement('a');
    link.className = 'ux-skip-link';
    link.href = '#timeline';
    link.textContent = t("ui.jumpToDiscussionTimeline");
    body.prepend(link);
  }

  function installAnnouncer() {
    const live = document.createElement('div');
    live.className = 'visually-hidden';
    live.id = 'ux-layout-announcer';
    live.setAttribute('role', 'status');
    live.setAttribute('aria-live', 'polite');
    live.setAttribute('aria-atomic', 'true');
    body.append(live);
    return live;
  }

  function announce(message) {
    announcer.textContent = '';
    window.setTimeout(() => { announcer.textContent = message; }, 20);
  }

  function installLayoutMenu() {
    topbarActions.classList.add('ux-topbar-actions');

    const button = document.createElement('button');
    button.id = 'ux-layout-button';
    button.className = 'icon-button ux-layout-button';
    button.type = 'button';
    button.textContent = '☷';
    button.title = t("ui.layoutDensityAndShortcuts");
    button.setAttribute('aria-label', t("ui.openLayoutDensityAndShortcutsMenu"));
    button.setAttribute('aria-haspopup', 'dialog');
    button.setAttribute('aria-expanded', 'false');
    button.setAttribute('aria-controls', 'ux-layout-menu');

    const connection = document.getElementById('connection');
    topbarActions.insertBefore(button, connection || topbarActions.firstChild);

    const menu = document.createElement('section');
    menu.id = 'ux-layout-menu';
    menu.className = 'ux-layout-menu';
    menu.hidden = true;
    menu.setAttribute('role', 'dialog');
    menu.setAttribute('aria-label', t("ui.roomLayoutSettings"));
    menu.innerHTML = "\n      <header class=\"ux-menu-header\">\n        <div>\n          <strong data-i18n=\"ui.roomLayout\">room layout</strong>\n          <span data-i18n=\"ui.onlySaveInCurrentBrowser\">Only save in current browser</span>\n        </div>\n        <button class=\"ux-menu-close\" type=\"button\" aria-label=\"Close layout menu\" data-i18n-aria-label=\"ui.closeLayoutMenu\">×</button>\n      </header>\n      <div class=\"ux-menu-section\" aria-label=\"Panel display\" data-i18n-aria-label=\"ui.panelDisplay\">\n        <button class=\"ux-menu-row\" type=\"button\" data-ux-action=\"participants\" aria-pressed=\"true\">\n          <span class=\"ux-menu-copy\"><strong data-i18n=\"ui.playersAndStrategies\">Players and Strategies</strong><small data-i18n=\"ui.agentStatusRoutingAndGitSummary\">Agent status, routing and Git summary</small></span>\n          <span class=\"ux-switch\" aria-hidden=\"true\"></span>\n        </button>\n        <button class=\"ux-menu-row\" type=\"button\" data-ux-action=\"inspector\" aria-pressed=\"true\">\n          <span class=\"ux-menu-copy\"><strong data-i18n=\"ui.workInspector\">Work inspector</strong><small data-i18n=\"ui.activitiesDiffsAndApprovals\">Activities, Diffs and Approvals</small></span>\n          <span class=\"ux-switch\" aria-hidden=\"true\"></span>\n        </button>\n        <button class=\"ux-menu-row\" type=\"button\" data-ux-action=\"focus\" aria-pressed=\"false\">\n          <span class=\"ux-menu-copy\"><strong data-i18n=\"ui.focusOnDiscussion\">focus on discussion</strong><small data-i18n=\"ui.temporarilyHideSidePanels\">Temporarily hide side panels</small></span>\n          <kbd>Ctrl/⌘ ⇧ F</kbd>\n        </button>\n      </div>\n      <div class=\"ux-menu-section\">\n        <div class=\"ux-menu-label\" data-i18n=\"ui.informationDensity\">information density</div>\n        <div class=\"ux-segmented\" role=\"group\" aria-label=\"information density\" data-i18n-aria-label=\"ui.informationDensity\">\n          <button type=\"button\" data-ux-density=\"comfortable\" data-i18n=\"ui.comfortable\">Comfortable</button>\n          <button type=\"button\" data-ux-density=\"compact\" data-i18n=\"ui.compact\">Compact</button>\n        </div>\n      </div>\n      <div class=\"ux-shortcuts\" aria-label=\"shortcut key\" data-i18n-aria-label=\"ui.shortcutKey\">\n        <span><kbd>Ctrl/⌘ K</kbd><span data-i18n=\"ui.searchDiscussion\"> Search discussion</span></span>\n        <span><kbd>c</kbd><span data-i18n=\"ui.focusInputBox\"> Focus input box</span></span>\n        <span><kbd>[</kbd><span data-i18n=\"ui.participants\"> Participants</span></span>\n        <span><kbd>]</kbd><span data-i18n=\"ui.inspector\"> inspector</span></span>\n        <span><kbd>?</kbd><span data-i18n=\"ui.thisMenu\"> this menu</span></span>\n      </div>\n      <footer class=\"ux-menu-footer\">\n        <button class=\"ux-reset-layout\" type=\"button\" data-ux-action=\"reset\" data-i18n=\"ui.restoreDefaultLayout\">Restore default layout</button>\n      </footer>";
    topbarActions.append(menu);

    button.addEventListener('click', () => setMenuOpen(!menuOpen));
    menu.querySelector('.ux-menu-close').addEventListener('click', () => setMenuOpen(false, { restoreFocus: true }));
    menu.addEventListener('click', (event) => {
      const action = event.target.closest('[data-ux-action]')?.dataset.uxAction;
      const density = event.target.closest('[data-ux-density]')?.dataset.uxDensity;
      if (action) handleMenuAction(action);
      if (density) setDensity(density);
    });
    document.addEventListener('pointerdown', (event) => {
      if (menuOpen && !menu.contains(event.target) && event.target !== button) setMenuOpen(false);
    });

    return { button, menu };
  }

  function setMenuOpen(open, { restoreFocus = false } = {}) {
    menuOpen = Boolean(open);
    layoutUI.button.setAttribute('aria-expanded', String(menuOpen));
    layoutUI.menu.hidden = !menuOpen;
    body.classList.toggle('ux-layout-menu-open', menuOpen);
    if (menuOpen) {
      lastFocusedElement = document.activeElement;
      requestAnimationFrame(() => layoutUI.menu.querySelector('button')?.focus({ preventScroll: true }));
    } else if (restoreFocus) {
      (lastFocusedElement instanceof HTMLElement ? lastFocusedElement : layoutUI.button).focus({ preventScroll: true });
    }
  }

  function handleMenuAction(action) {
    switch (action) {
      case 'participants':
        togglePanel('participants');
        break;
      case 'inspector':
        togglePanel('inspector');
        break;
      case 'focus':
        setFocusMode(!state.focusMode);
        break;
      case 'reset':
        Object.assign(state, DEFAULTS, { mobilePanel: '' });
        applyLayout({ announcement: t("ui.defaultRoomLayoutRestored") });
        break;
      default:
        break;
    }
  }

  function installDrawerBackdrop() {
    const element = document.createElement('button');
    element.className = 'ux-drawer-backdrop';
    element.type = 'button';
    element.setAttribute('aria-label', t("ui.closeSidePanel"));
    element.addEventListener('click', closeMobilePanel);
    workspace.append(element);
    return element;
  }

  function installPanelRail(panel, label, symbol) {
    const element = document.createElement('button');
    element.className = `ux-panel-rail ux-panel-rail-${panel}`;
    element.type = 'button';
    element.textContent = symbol;
    element.setAttribute('aria-label', label);
    element.title = label;
    element.addEventListener('click', () => setPanelVisible(panel, true));
    workspace.append(element);
    return element;
  }

  function installResizer(panel) {
    const element = document.createElement('div');
    element.className = `ux-resizer ux-resizer-${panel}`;
    element.tabIndex = 0;
    element.setAttribute('role', 'separator');
    element.setAttribute('aria-orientation', 'vertical');
    element.setAttribute('aria-label', panel === 'participants' ? t("ui.resizeParticipantsPanel") : t("ui.resizeWorkInspector"));
    element.setAttribute('aria-valuemin', String(LIMITS[panel][0]));
    element.setAttribute('aria-valuemax', String(LIMITS[panel][1]));

    if (panel === 'participants') workspace.insertBefore(element, chatPanel);
    else workspace.insertBefore(element, inspectorPanel);

    element.addEventListener('pointerdown', (event) => startResize(panel, event, element));
    element.addEventListener('dblclick', () => {
      state[widthKey(panel)] = DEFAULTS[widthKey(panel)];
      applyWidths();
      storeState();
      announce(t("ui.valueWidthWasReset", { value0: (panel === 'participants' ? t('ui.participantsPanel') : t('ui.workInspector')) }));
    });
    element.addEventListener('keydown', (event) => resizeWithKeyboard(panel, event));
    return element;
  }

  function widthKey(panel) {
    return panel === 'participants' ? 'participantsWidth' : 'inspectorWidth';
  }

  function startResize(panel, event, handle) {
    if (!isDesktop() || state.focusMode || !panelVisible(panel)) return;
    if (event.button !== 0) return;
    event.preventDefault();
    const key = widthKey(panel);
    const startX = event.clientX;
    const startWidth = renderedPanelWidth(panel);
    state[key] = startWidth;
    body.classList.add('ux-resizing');
    handle.setPointerCapture?.(event.pointerId);

    const move = (moveEvent) => {
      const delta = moveEvent.clientX - startX;
      const proposed = panel === 'participants' ? startWidth + delta : startWidth - delta;
      state[key] = clamp(proposed, ...LIMITS[panel]);
      if (!resizeFrame) {
        resizeFrame = requestAnimationFrame(() => {
          resizeFrame = 0;
          applyWidths();
        });
      }
    };
    const stop = () => {
      body.classList.remove('ux-resizing');
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', stop);
      handle.removeEventListener('pointercancel', stop);
      storeState();
      announce(t("ui.valueWidthValuePixels", { value0: (panel === 'participants' ? t('ui.participantsPanel') : t('ui.workInspector')), value1: (renderedPanelWidth(panel)) }));
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', stop);
    handle.addEventListener('pointercancel', stop);
  }

  function resizeWithKeyboard(panel, event) {
    if (!isDesktop() || !['ArrowLeft', 'ArrowRight', 'Home'].includes(event.key)) return;
    event.preventDefault();
    const key = widthKey(panel);
    if (event.key === 'Home') state[key] = DEFAULTS[key];
    else {
      const direction = event.key === 'ArrowRight' ? 1 : -1;
      const delta = panel === 'participants' ? direction * 16 : direction * -16;
      state[key] = clamp(renderedPanelWidth(panel) + delta, ...LIMITS[panel]);
    }
    applyWidths();
    storeState();
    announce(t("ui.valueWidthValuePixels", { value0: (panel === 'participants' ? t('ui.participantsPanel') : t('ui.workInspector')), value1: (renderedPanelWidth(panel)) }));
  }

  function renderedPanelWidth(panel) {
    const target = panel === 'participants' ? participantsPanel : inspectorPanel;
    return clamp(Math.round(target.getBoundingClientRect().width), ...LIMITS[panel]);
  }

  function applyWidths() {
    root.style.setProperty('--ux-participants-width', `${Math.round(state.participantsWidth)}px`);
    root.style.setProperty('--ux-inspector-width', `${Math.round(state.inspectorWidth)}px`);
    leftResizer?.setAttribute('aria-valuenow', String(renderedPanelWidth('participants')));
    rightResizer?.setAttribute('aria-valuenow', String(renderedPanelWidth('inspector')));
  }

  function panelVisible(panel) {
    return panel === 'participants' ? state.participantsVisible : state.inspectorVisible;
  }

  function setPanelVisible(panel, visible, { announcement = true } = {}) {
    const target = panel === 'participants' ? participantsPanel : inspectorPanel;
    if (!visible && target.contains(document.activeElement)) (timeline || messageInput)?.focus({ preventScroll: true });
    if (panel === 'participants') state.participantsVisible = Boolean(visible);
    else state.inspectorVisible = Boolean(visible);
    if (visible && state.focusMode) state.focusMode = false;
    state.mobilePanel = '';
    applyLayout({ announcement: announcement ? t("ui.valueIsNowValue", { value0: (panel === 'participants' ? t('ui.participantsPanel') : t('ui.workInspector')), value1: (visible ? t('ui.show') : t('ui.hide')) }) : '' });
  }

  function togglePanel(panel) {
    if (isDesktop()) {
      setPanelVisible(panel, !panelVisible(panel));
      return;
    }
    const opening = state.mobilePanel !== panel;
    if (opening) drawerReturnFocus = document.activeElement;
    state.focusMode = false;
    state.mobilePanel = opening ? panel : '';
    if (menuOpen) setMenuOpen(false);
    applyLayout({ persist: false, announcement: state.mobilePanel ? t("ui.openedValue", { value0: (panel === 'participants' ? t('ui.participantsPanel') : t('ui.workInspector')) }) : t("ui.sidePanelClosed") });
    if (state.mobilePanel) {
      const target = panel === 'participants' ? participantsPanel : inspectorPanel;
      const focusPanel = () => {
        if (!isDesktop() && state.mobilePanel === panel) target.focus({ preventScroll: true });
      };
      requestAnimationFrame(focusPanel);
      window.setTimeout(focusPanel, 230);
    } else {
      restoreDrawerFocus();
    }
  }

  function closeMobilePanel() {
    if (!state.mobilePanel) return;
    state.mobilePanel = '';
    applyLayout({ persist: false, announcement: t("ui.sidePanelClosed") });
    restoreDrawerFocus();
  }

  function restoreDrawerFocus() {
    const target = drawerReturnFocus;
    drawerReturnFocus = null;
    const usable = target instanceof HTMLElement && target.isConnected && !target.closest('[hidden]') &&
      (target.tabIndex >= 0 || target.isContentEditable);
    const focusTarget = usable ? target : layoutUI.button;
    focusTarget.focus({ preventScroll: true });
    if (document.activeElement !== focusTarget) {
      requestAnimationFrame(() => focusTarget.focus({ preventScroll: true }));
    }
  }

  function setFocusMode(enabled) {
    state.focusMode = Boolean(enabled);
    state.mobilePanel = '';
    if (state.focusMode && (participantsPanel.contains(document.activeElement) || inspectorPanel.contains(document.activeElement))) {
      (timeline || messageInput)?.focus({ preventScroll: true });
    }
    applyLayout({ announcement: state.focusMode ? t("ui.focusModeEnabled") : t("ui.focusModeDisabled") });
  }

  function setDensity(value) {
    state.density = value === 'compact' ? 'compact' : 'comfortable';
    applyLayout({ announcement: t("ui.densityChangedToValue", { value0: (state.density === 'compact' ? t('ui.compact') : t('ui.comfortable')) }) });
  }

  function applyLayout({ persist = true, announcement = '' } = {}) {
    applyWidths();
    const desktop = isDesktop();
    const hideParticipants = desktop && (state.focusMode || !state.participantsVisible);
    const hideInspector = desktop && (state.focusMode || !state.inspectorVisible);

    body.classList.toggle('ux-focus-mode', state.focusMode);
    body.classList.toggle('ux-density-compact', state.density === 'compact');
    body.classList.toggle('ux-participants-hidden', hideParticipants);
    body.classList.toggle('ux-inspector-hidden', hideInspector);
    body.classList.toggle('ux-participants-open', !desktop && state.mobilePanel === 'participants');
    body.classList.toggle('ux-inspector-open', !desktop && state.mobilePanel === 'inspector');
    body.classList.toggle('ux-drawer-open', !desktop && Boolean(state.mobilePanel));

    setPanelAccessibility(participantsPanel, desktop ? hideParticipants : state.mobilePanel !== 'participants');
    setPanelAccessibility(inspectorPanel, desktop ? hideInspector : state.mobilePanel !== 'inspector');
    backdrop.tabIndex = !desktop && state.mobilePanel ? 0 : -1;

    leftRail.hidden = !desktop || !hideParticipants || state.focusMode;
    rightRail.hidden = !desktop || !hideInspector || state.focusMode;
    leftResizer.hidden = !desktop || hideParticipants;
    rightResizer.hidden = !desktop || hideInspector;

    syncLayoutControls();
    if (persist) storeState();
    if (announcement) announce(announcement);
  }

  function setPanelAccessibility(panel, hidden) {
    panel.setAttribute('aria-hidden', String(hidden));
    if ('inert' in panel) panel.inert = hidden;
  }

  function syncLayoutControls() {
    const participantControl = layoutUI.menu.querySelector('[data-ux-action="participants"]');
    const inspectorControl = layoutUI.menu.querySelector('[data-ux-action="inspector"]');
    const focusControl = layoutUI.menu.querySelector('[data-ux-action="focus"]');
    const participantOn = isDesktop() ? state.participantsVisible && !state.focusMode : state.mobilePanel === 'participants';
    const inspectorOn = isDesktop() ? state.inspectorVisible && !state.focusMode : state.mobilePanel === 'inspector';

    participantControl.setAttribute('aria-pressed', String(participantOn));
    inspectorControl.setAttribute('aria-pressed', String(inspectorOn));
    focusControl.setAttribute('aria-pressed', String(state.focusMode));
    participantControl.classList.toggle('active', participantOn);
    inspectorControl.classList.toggle('active', inspectorOn);
    focusControl.classList.toggle('active', state.focusMode);
    layoutUI.button.classList.toggle('active', state.focusMode || !state.participantsVisible || !state.inspectorVisible || state.density === 'compact');

    layoutUI.menu.querySelectorAll('[data-ux-density]').forEach((button) => {
      const active = button.dataset.uxDensity === state.density;
      button.classList.toggle('active', active);
      button.setAttribute('aria-pressed', String(active));
    });
  }

  function isDesktop() {
    return !MOBILE_QUERY.matches;
  }

  function improveSemantics() {
    participantsPanel.tabIndex = -1;
    participantsPanel.setAttribute('aria-label', t("ui.participantsTurnPolicyAndWorkspaceStatus"));
    inspectorPanel.tabIndex = -1;
    inspectorPanel.setAttribute('aria-label', t("ui.workInspector"));
    chatPanel.setAttribute('aria-label', t("ui.sharedDiscussionRoom"));

    if (timeline) {
      timeline.tabIndex = 0;
      timeline.setAttribute('aria-label', t("ui.discussionTimeline"));
      timeline.setAttribute('role', 'log');
      timeline.setAttribute('aria-relevant', 'additions text');
    }
    if (messageInput) messageInput.setAttribute('aria-label', t("ui.messageToTheRoom"));
    if (messageSearch) {
      messageSearch.setAttribute('aria-label', t("ui.searchDiscussion"));
      messageSearch.setAttribute('aria-keyshortcuts', 'Control+K Meta+K');
    }
    const attachmentButton = document.getElementById('attach-button');
    if (attachmentButton) attachmentButton.setAttribute('aria-label', t("ui.addImageAttachment"));
    const connection = document.getElementById('connection');
    if (connection) {
      connection.setAttribute('role', 'status');
      connection.setAttribute('aria-live', 'polite');
    }

    document.querySelectorAll('button:not([type])').forEach((button) => { button.type = 'button'; });
    installTabSemantics();
  }

  function installTabSemantics() {
    const tabs = Array.from(document.querySelectorAll('.tabs [data-tab]'));
    if (!tabs.length) return;
    const sync = () => {
      tabs.forEach((tab, index) => {
        const name = tab.dataset.tab;
        const panel = document.getElementById(`${name}-tab`);
        const active = tab.classList.contains('active');
        tab.id ||= `ux-${name}-tab`;
        tab.setAttribute('role', 'tab');
        tab.setAttribute('aria-controls', panel?.id || '');
        tab.setAttribute('aria-selected', String(active));
        tab.tabIndex = active ? 0 : -1;
        if (panel) {
          panel.setAttribute('role', 'tabpanel');
          panel.setAttribute('aria-labelledby', tab.id);
          panel.tabIndex = 0;
        }
        if (!tab.dataset.uxKeyboardBound) {
          tab.dataset.uxKeyboardBound = 'true';
          tab.addEventListener('keydown', (event) => {
            if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
            event.preventDefault();
            let next = index;
            if (event.key === 'Home') next = 0;
            else if (event.key === 'End') next = tabs.length - 1;
            else next = (index + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length;
            tabs[next].focus();
            tabs[next].click();
          });
        }
      });
    };
    sync();
    const observer = new MutationObserver(sync);
    tabs.forEach((tab) => observer.observe(tab, { attributes: true, attributeFilter: ['class'] }));
  }

  function installKeyboardNavigation() {
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        if (menuOpen) {
          event.preventDefault();
          setMenuOpen(false, { restoreFocus: true });
          return;
        }
        if (!isDesktop() && state.mobilePanel) {
          event.preventDefault();
          closeMobilePanel();
          return;
        }
      }

      if (document.querySelector('dialog[open]')) return;
      if (!isDesktop() && state.mobilePanel && event.key === 'Tab' && trapDrawerFocus(event)) return;

      const modifier = event.ctrlKey || event.metaKey;
      if (modifier && event.shiftKey && event.key.toLowerCase() === 'f') {
        event.preventDefault();
        setFocusMode(!state.focusMode);
        return;
      }
      if (isEditableTarget(event.target) || modifier || event.altKey) return;

      if (event.key === '[') {
        event.preventDefault();
        togglePanel('participants');
      } else if (event.key === ']') {
        event.preventDefault();
        togglePanel('inspector');
      } else if (event.key === '?') {
        event.preventDefault();
        setMenuOpen(true);
      } else if (event.key === 'c' && messageInput) {
        event.preventDefault();
        messageInput.focus();
      } else if (event.key === '/' && messageSearch) {
        event.preventDefault();
        messageSearch.focus();
      }
    });
  }

  function isEditableTarget(target) {
    return target instanceof HTMLElement && (target.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName));
  }

  function trapDrawerFocus(event) {
    const panel = state.mobilePanel === 'participants' ? participantsPanel : inspectorPanel;
    const focusable = Array.from(panel.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'))
      .filter((element) => !element.closest('[hidden]') && element.getClientRects().length > 0);
    if (!focusable.length) {
      event.preventDefault();
      panel.focus({ preventScroll: true });
      return true;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (!panel.contains(document.activeElement)) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus({ preventScroll: true });
      return true;
    }
    if (document.activeElement === panel) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus({ preventScroll: true });
      return true;
    }
    if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus({ preventScroll: true });
      return true;
    }
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus({ preventScroll: true });
      return true;
    }
    return false;
  }

  function installTimelineAttention() {
    if (!timeline || !scrollBottom) return;
    let atBottom = true;
    const syncAttention = () => {
      const hasUnread = /\(\d+\)/.test(scrollBottom.textContent || '');
      scrollBottom.classList.toggle('ux-attention', !atBottom && hasUnread);
      if (!atBottom && hasUnread) scrollBottom.setAttribute('aria-label', t("ui.newMessagesJumpToLatest"));
      else scrollBottom.removeAttribute('aria-label');
    };
    const update = () => {
      atBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 72;
      syncAttention();
    };
    timeline.addEventListener('scroll', update, { passive: true });
    scrollBottom.addEventListener('click', () => window.setTimeout(clearTimelineAttention, 120));
    const observer = new MutationObserver(syncAttention);
    observer.observe(scrollBottom, { childList: true, characterData: true, subtree: true });
    update();
  }

  function clearTimelineAttention() {
    if (!scrollBottom) return;
    scrollBottom.classList.remove('ux-attention');
    scrollBottom.removeAttribute('aria-label');
  }

  function installResponsiveListeners() {
    const handleChange = () => {
      state.mobilePanel = '';
      setMenuOpen(false);
      applyLayout({ persist: false });
    };
    if (typeof MOBILE_QUERY.addEventListener === 'function') MOBILE_QUERY.addEventListener('change', handleChange);
    else MOBILE_QUERY.addListener(handleChange);

    window.addEventListener('resize', () => {
      window.clearTimeout(installResponsiveListeners.resizeTimer);
      installResponsiveListeners.resizeTimer = window.setTimeout(() => applyLayout({ persist: false }), 100);
    }, { passive: true });
  }
})();
