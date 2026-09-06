(() => {
  'use strict';

  const t = (key, options) => window.PairRoomI18n ? window.PairRoomI18n.t(key, options) : key;

  const bootstrapParameters = new URLSearchParams(window.location.hash.replace(/^#/, ''));
  let bootstrapToken = bootstrapParameters.get('token') || '';
  if (bootstrapToken) {
    // Fragments are not sent in HTTP requests or Referer headers. Remove the
    // one-time bootstrap secret from the address bar immediately and keep it
    // only in memory until it is exchanged for an HttpOnly browser session.
    history.replaceState(null, '', `${window.location.pathname}${window.location.search}`);
  }
  const STREAM_RENDER_INTERVAL_MS = 50;
  const SURFACE_PREFIX = (() => {
    const match = window.location.pathname.match(/^(.*\/api\/v1\/rooms\/[^/]+\/surface)(?:\/|$)/);
    return match ? match[1].replace(/\/$/, '') : '';
  })();
  const EMBEDDED = Boolean(SURFACE_PREFIX);
  if (EMBEDDED) {
    document.documentElement.dataset.embed = '1';
    if (document.body) document.body.dataset.embed = '1';
  }
  const state = {
    snapshot: null,
    drafts: { claude: '', codex: '' },
    draftCorrelation: { claude: '', codex: '' },
    selectedTarget: 'claude',
    permissionSubmitting: new Set(),
    participantActions: new Set(),
    messageActions: new Set(),
    settingsDirty: false,
    settingsRevision: 0,
    settingsSaving: false,
    gitStatusRequest: 0,
    diffRequest: 0,
    replyTo: '',
    replyRevision: 0,
    draftRevision: 0,
    composing: false,
    sending: false,
    approvalSubmissions: new Map(),
    localRoomID: '',
    pendingAttachments: [],
    attachmentObjectURLs: new Set(),
    mediaObjectURLs: new Map(),
    lightboxItems: [],
    lightboxIndex: -1,
    lightboxRequest: 0,
    lightboxZoom: 1,
    lightboxRotation: 0,
    lightboxMode: 'fit',
    loadingOlder: false,
    unreadCount: 0,
    lastSeenSeq: 0,
    draftKey: '',
    expandedMessages: new Set(),
    conversationFilter: 'all',
    threadFilter: '',
    inspectorAgent: 'all',
    inspectorTab: 'activity',
    inspectorCorrelation: '',
    searchQuery: '',
    shellActive: true,
    source: null,
    snapshotPromise: null,
    reconnectTimer: null,
    reconnectAttempt: 0,
    renderQueued: false,
    streamRenderTimer: null,
    runtimeRenderQueued: false,
    runtimeRenderScopes: new Set(),
    runtimeMessageRenderIDs: new Set(),
    csrfToken: '',
  };

  const $ = (id) => document.getElementById(id);
  const timeline = $('timeline');
  const messageInput = $('message-input');

  async function initializeSession() {
    const headers = new Headers();
    let method = 'GET';
    if (bootstrapToken) {
      method = 'POST';
      headers.set('Authorization', `Bearer ${bootstrapToken}`);
    }
    const response = await fetch(roomURL('/api/v1/session'), { method, headers, credentials: 'same-origin' });
    const payload = await response.json().catch(() => ({}));
    bootstrapToken = '';
    if (!response.ok) {
	  const message = window.PairRoomI18n?.errorMessage(payload) || payload.error || (response.status === 401
        ? t("ui.theBrowserSessionIsInvalidReopenTheFullUrlFromPairroomStartup")
        : response.statusText);
      throw new Error(message);
    }
    state.csrfToken = payload.csrf_token || '';
  }

  function roomURL(path) {
    const value = String(path || '');
    if (!value.startsWith('/')) return SURFACE_PREFIX + '/' + value;
    return SURFACE_PREFIX + value;
  }

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    const method = String(options.method || 'GET').toUpperCase();
    if (!['GET', 'HEAD', 'OPTIONS'].includes(method) && state.csrfToken) headers.set('X-PairRoom-CSRF', state.csrfToken);
    if (options.body && !(options.body instanceof FormData) && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await fetch(roomURL(path), { ...options, method, headers, credentials: 'same-origin' });
    const type = response.headers.get('content-type') || '';
    const payload = type.includes('application/json') ? await response.json() : await response.text();
    if (!response.ok) {
	  const message = window.PairRoomI18n?.errorMessage(payload) || (payload && payload.error ? payload.error : String(payload || response.statusText));
      throw new Error(message);
    }
    return payload;
  }

  async function apiBlob(path) {
    const response = await fetch(roomURL(path), { credentials: 'same-origin' });
    if (!response.ok) {
      const payload = await response.json().catch(() => ({}));
	  throw new Error(window.PairRoomI18n?.errorMessage(payload) || payload.error || response.statusText);
    }
    return response.blob();
  }

  function queueRender() {
    if (state.renderQueued) return;
    state.renderQueued = true;
    requestAnimationFrame(() => {
      state.renderQueued = false;
      render();
    });
  }

  function queueStreamingRender() {
    if (state.streamRenderTimer) return;
    state.streamRenderTimer = setTimeout(() => {
      state.streamRenderTimer = null;
      requestAnimationFrame(renderStreamingDrafts);
    }, STREAM_RENDER_INTERVAL_MS);
  }

  function queueRuntimeRender(scopes = ['participants', 'drafts', 'activity'], messageID = '') {
    scopes.forEach((scope) => state.runtimeRenderScopes.add(scope));
    if (messageID) state.runtimeMessageRenderIDs.add(messageID);
    if (state.runtimeRenderQueued) return;
    state.runtimeRenderQueued = true;
    requestAnimationFrame(() => {
      if (!state.runtimeRenderQueued) return;
      state.runtimeRenderQueued = false;
      const pending = new Set(state.runtimeRenderScopes);
      const messageIDs = Array.from(state.runtimeMessageRenderIDs);
      state.runtimeRenderScopes.clear();
      state.runtimeMessageRenderIDs.clear();
      if (pending.has('participants')) renderParticipants();
      if (pending.has('settings')) renderSettings();
      if (pending.has('drafts')) renderStreamingDrafts();
      if (pending.has('activity')) renderActivity();
      if (pending.has('approvals')) renderApprovals();
      if (pending.has('created')) messageIDs.forEach(renderCreatedMessage);
      if (pending.has('messages')) messageIDs.forEach(renderMessage);
      if (pending.has('delivery')) { updateDeliveryHint(); renderTurnOwnerBar(); }
      if (pending.has('composer')) updateComposerAvailability();
    });
  }

  // Reads may be retried; message submissions must never be replayed implicitly.
  // Closing the old stream before the read also discards its queued telemetry.
  function loadSnapshot() {
    if (state.snapshotPromise) return state.snapshotPromise;
    const initial = !state.snapshot;
    closeEvents();
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
    state.snapshotPromise = (async () => {
      const limit = Math.min(1000, Math.max(250, state.snapshot?.messages?.length || 0));
      state.snapshot = await api(`/api/v1/snapshot?message_limit=${limit}`);
      state.drafts = { claude: '', codex: '' };
      state.draftCorrelation = { claude: '', codex: '' };
      initializeRoomLocalState();
      if (state.snapshot?.meta?.id) document.body.dataset.roomId = state.snapshot.meta.id;
      render(initial);
      connectEvents();
      refreshGitStatus();
      postSurfaceState();
    })().catch((error) => {
      if (!initial) scheduleReconnect();
      throw error;
    }).finally(() => { state.snapshotPromise = null; });
    return state.snapshotPromise;
  }

  function closeEvents() {
    if (state.source) state.source.close();
    state.source = null;
  }

  function scheduleReconnect() {
    setConnection(false, 'room.reconnecting');
    if (state.reconnectTimer !== null) return;
    const delay = Math.min(15000, 500 * (2 ** Math.min(state.reconnectAttempt++, 5)));
    state.reconnectTimer = setTimeout(() => {
      state.reconnectTimer = null;
      void loadSnapshot().catch(() => {}); // loadSnapshot schedules the next read.
    }, delay);
  }

  function connectEvents() {
    closeEvents();
    const since = state.snapshot ? state.snapshot.latest_seq || 0 : 0;
    const query = new URLSearchParams({ since: String(since) });
    const source = new EventSource(roomURL(`/api/v1/events?${query}`));
    state.source = source;
    setConnection(false, 'ui.connecting');
    source.addEventListener('open', () => {
      if (state.source !== source) return;
      state.reconnectAttempt = 0;
      setConnection(true, 'room.live');
    });
    source.addEventListener('error', () => {
      if (state.source !== source) return;
      closeEvents();
      scheduleReconnect();
    });
    source.addEventListener('reset', () => {
      if (state.source !== source) return;
      void loadSnapshot().catch(() => {});
    });
    source.addEventListener('pairroom', (raw) => {
      if (state.source !== source) return;
      try {
        applyEvent(JSON.parse(raw.data));
      } catch (error) {
        toast(t("ui.couldNotParseEventValue", { value0: error.message }), 'error');
        void loadSnapshot().catch(() => {});
      }
    });
  }

  function applyEvent(event) {
    if (!state.snapshot) return;
    const durable = Number(event.seq || 0) > 0;
    const latest = Number(state.snapshot.latest_seq || 0);
    if (durable && event.seq <= latest) return;
    if (durable && event.seq > latest + 1) {
      toast(t("ui.eventSequenceGapValueValueResynchronizing", { value0: (latest), value1: (event.seq) }), 'error');
      loadSnapshot().catch((error) => toast(error.message, 'error'));
      return;
    }
    if (durable) state.snapshot.latest_seq = event.seq;
    state.snapshot.events = state.snapshot.events || [];
    // Streaming text is accumulated in drafts. It is not diagnostic history
    // and must not evict useful tool/approval events on every generated token.
    if (!(event.kind === 'runtime.event' && event.data?.kind === 'text.delta')) state.snapshot.events.push(event);
    if (state.snapshot.events.length > 600) state.snapshot.events.splice(0, state.snapshot.events.length - 600);
    const data = event.data || {};
    let renderScope = 'full';
    let renderMessageID = '';

    switch (event.kind) {
      case 'room.settings.updated':
        state.snapshot.settings = data;
        renderScope = 'settings';
        break;
      case 'participant.permissions.updated':
        if (state.snapshot.participants[data.actor]) state.snapshot.participants[data.actor].permission_profile = data.profile;
        renderScope = 'participants';
        break;
      case 'participant.updated':
        state.snapshot.participants[data.id] = data;
        renderScope = 'participants';
        break;
      case 'message.created': {
        if (!state.snapshot.messages.some((item) => item.id === data.id)) {
          data.seq = event.seq;
          state.snapshot.messages.push(data);
          if (state.snapshot.message_window) {
            state.snapshot.message_window.total = Number(state.snapshot.message_window.total || 0) + 1;
            state.snapshot.message_window.loaded = state.snapshot.messages.length;
            if (!state.snapshot.message_window.oldest_seq) state.snapshot.message_window.oldest_seq = data.seq;
          }
          handleIncomingMessage(data, event.seq);
        }
        if (data.from === 'claude' || data.from === 'codex') {
          state.drafts[data.from] = '';
          state.draftCorrelation[data.from] = '';
        }
        if (data.retry_of) queueRuntimeRender(['messages'], data.retry_of);
        renderScope = 'message-created';
        renderMessageID = data.id || '';
        break;
      }
      case 'message.delivery.updated': {
        const message = state.snapshot.messages.find((item) => item.id === data.message_id);
        if (message) {
          message.delivery = message.delivery || {};
          message.delivery_detail = message.delivery_detail || {};
          const current = message.delivery[data.target] || '';
          if (deliveryTransitionAllowed(current, data.state)) {
            message.delivery[data.target] = data.state;
            message.delivery_detail[data.target] = data.detail || '';
          }
        }
        renderScope = 'message';
        renderMessageID = data.message_id || '';
        break;
      }
      case 'message.processing.updated': {
        const message = state.snapshot.messages.find((item) => item.id === data.message_id);
        if (message) {
          message.processing = message.processing || {};
          message.processing_detail = message.processing_detail || {};
          message.processing_turn = message.processing_turn || {};
          message.processing_last_updated_at = message.processing_last_updated_at || {};
          if (processingTransitionAllowed(message.processing[data.target], data.state)) {
            message.processing[data.target] = data.state;
            message.processing_detail[data.target] = data.detail || '';
            message.processing_turn[data.target] = data.turn_id || '';
            message.processing_last_updated_at[data.target] = data.updated_at || new Date().toISOString();
          }
          if (message.retry_of) queueRuntimeRender(['messages'], message.retry_of);
        }
        renderScope = 'message';
        renderMessageID = data.message_id || '';
        break;
      }
      case 'turn.summary.updated': {
        state.snapshot.turns = state.snapshot.turns || [];
        const index = state.snapshot.turns.findIndex((item) => item.id === data.id);
        if (index >= 0) state.snapshot.turns[index] = data;
        else state.snapshot.turns.push(data);
        renderScope = 'activity';
        break;
      }
      case 'approval.updated': {
        state.snapshot.approvals = state.snapshot.approvals || [];
        const index = state.snapshot.approvals.findIndex((item) => item.id === data.id);
        if (index >= 0) state.snapshot.approvals[index] = data;
        else state.snapshot.approvals.push(data);
        renderScope = 'approvals';
        break;
      }
      case 'runtime.event':
        applyRuntime(data);
        renderScope = data.kind === 'text.delta' ? 'stream' : 'runtime';
        break;
      case 'system.notice':
        if (data.level === 'error') toast(data.text, 'error');
        break;
      default:
        break;
    }
    if (renderScope === 'stream') queueStreamingRender();
    else if (renderScope === 'runtime') queueRuntimeRender();
    else if (renderScope === 'settings') queueRuntimeRender(['settings']);
    else if (renderScope === 'participants') queueRuntimeRender(['participants', 'composer']);
    else if (renderScope === 'message-created') queueRuntimeRender(['created', 'delivery'], renderMessageID);
    else if (renderScope === 'message') queueRuntimeRender(['messages', 'delivery'], renderMessageID);
    else if (renderScope === 'activity') queueRuntimeRender(['activity']);
    else if (renderScope === 'approvals') queueRuntimeRender(['approvals', 'composer']);
    else queueRender();
  }

  function applyRuntime(runtime) {
    const actor = runtime.agent;
    if (state.snapshot.participants && state.snapshot.participants[actor]) {
      state.snapshot.participants[actor].last_activity = runtime.created_at || new Date().toISOString();
	  if (runtime.kind === 'runtime.info' && runtime.runtime) {
		state.snapshot.participants[actor].runtime = runtime.runtime;
		state.snapshot.participants[actor].runtime_kind = runtime.runtime.runtime_kind || state.snapshot.participants[actor].runtime_kind;
		state.snapshot.participants[actor].model = runtime.runtime.model || state.snapshot.participants[actor].model;
	  }
    }
    if (runtime.kind === 'turn.started') {
      state.drafts[actor] = '';
      state.draftCorrelation[actor] = runtime.correlation_id || '';
    }
    if (runtime.kind === 'text.delta') {
      if (runtime.correlation_id && state.draftCorrelation[actor] && state.draftCorrelation[actor] !== runtime.correlation_id) {
        state.drafts[actor] = '';
      }
      state.draftCorrelation[actor] = runtime.correlation_id || state.draftCorrelation[actor] || '';
      state.drafts[actor] = (state.drafts[actor] || '') + (runtime.text || '');
    }
    if (runtime.kind === 'error' && runtime.text) toast(`${displayName(actor)}：${runtime.text}`, 'error');
  }

  function deliveryTransitionAllowed(current, next) {
    if (!current) return true;
    if (current === 'failed' || current === 'skipped') return false;
    if (next === 'failed' || next === 'skipped') return true;
    switch (current) {
      case 'pending':
        return next === 'pending' || next === 'queued' || next === 'submitting';
      case 'queued':
        return next === 'queued' || next === 'submitting';
      case 'submitting':
        return ['submitting', 'queued', 'started', 'injected'].includes(next);
      default:
        return current === next;
    }
  }

  // Mirror the durable projection: a late receipt cannot revive settled work.
  function processingTransitionAllowed(current, next) {
    if (!next) return false;
    if (!current || current === 'waiting') return true;
    if (['completed', 'failed', 'cancelled'].includes(current)) return current === next;
    return ['completed', 'failed', 'cancelled'].includes(next) || current === next;
  }

  async function refreshAfterWrite() {
    if (state.snapshotPromise) await state.snapshotPromise.catch(() => {});
    await loadSnapshot();
  }

  function participantBusy(actor) {
    return state.participantActions.has(actor) || state.permissionSubmitting.has(actor);
  }

  function messageActionKey(id, target) { return JSON.stringify([id, target]); }

  function pendingRetry(id, target) {
    return (state.snapshot?.messages || []).some((message) => message.retry_of === id
      && ['waiting', 'working'].includes(message.processing?.[target]));
  }

  function render(forceBottom = false) {
    if (!state.snapshot) return;
    if (state.streamRenderTimer) {
      clearTimeout(state.streamRenderTimer);
      state.streamRenderTimer = null;
    }
    state.runtimeRenderQueued = false;
    state.runtimeRenderScopes.clear();
    state.runtimeMessageRenderIDs.clear();
    const nearBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 140;
    const scrollTop = timeline.scrollTop;
    const top = timeline.getBoundingClientRect().top;
    const anchor = !forceBottom && !nearBottom
      ? Array.from(timeline.querySelectorAll('[data-message-id]')).find((node) => node.getBoundingClientRect().bottom > top)
      : null;
    const anchorID = anchor?.dataset.messageId;
    const anchorOffset = anchor ? anchor.getBoundingClientRect().top - top : 0;
    $('room-name').textContent = state.snapshot.meta.name;
    $('repo-path').textContent = state.snapshot.meta.repo;
    const collaboration = state.snapshot.meta.collaboration;
    $('room-collaboration-label').textContent = t(collaboration?.mode === 'default' ? 'room.collaboration.default' : collaboration ? 'room.collaboration.custom' : 'room.collaboration.legacy');
    $('room-collaboration-instructions').textContent = collaboration?.instructions || t('room.collaboration.legacyHelp');
	const chatDescription = $('chat-description');
	if (chatDescription) chatDescription.textContent = ['You', displayName('claude'), displayName('codex')].join(' · ');
    renderParticipants();
    updateDeliveryHint();
    renderTurnOwnerBar();
    renderSettings();
    renderAttachmentStrip();
    updateComposerAvailability();
    renderTimeline();
    renderActivity();
    renderApprovals();
    if (forceBottom || nearBottom) requestAnimationFrame(scrollBottom);
    else requestAnimationFrame(() => {
      const current = anchorID && timeline.querySelector(`[data-message-id="${CSS.escape(anchorID)}"]`);
      if (current) timeline.scrollTop += current.getBoundingClientRect().top - timeline.getBoundingClientRect().top - anchorOffset;
      else timeline.scrollTop = scrollTop;
    });
  }

  function renderTurnOwnerBar() {
    const bar = $('turn-owner-bar');
    if (!bar || !state.snapshot) return;
    const messages = state.snapshot.messages || [];
    const live = ['submitting', 'started', 'injected'];
    let owner = '';
    for (const message of messages) {
      const delivery = message.delivery || {};
      const processing = message.processing || {};
      for (const target of Object.keys(delivery)) {
        if (live.includes(delivery[target]) && (processing[target] === 'working' || processing[target] === 'waiting')) {
          owner = target;
        }
      }
    }
    let queued = 0;
    for (const message of messages) {
      const delivery = message.delivery || {};
      const processing = message.processing || {};
      for (const target of Object.keys(delivery)) {
        if (delivery[target] === 'queued' && processing[target] === 'waiting') {
          queued += 1;
        }
      }
    }
    if (!owner && queued === 0) {
      bar.classList.add('hidden');
      bar.replaceChildren();
      return;
    }
    bar.classList.remove('hidden');
    const label = document.createElement('span');
    label.className = 'turn-owner-label';
    label.textContent = owner ? t("ui.currentTurnValue", { value0: (displayName(owner)) }) : t("ui.idle");
    bar.replaceChildren(label);
    if (queued > 0) {
      const badge = document.createElement('span');
      badge.className = 'turn-queue-count';
      badge.textContent = t("ui.queueValue", { value0: (queued) });
      bar.appendChild(badge);
    }
  }

  function renderParticipants() {
    const container = $('participants');
    container.replaceChildren();
    ['claude', 'codex'].forEach((actor) => {
      const p = state.snapshot.participants[actor] || { id: actor, state: 'stopped', role: 'peer' };
      const card = document.createElement('div');
      const stalled = participantLooksStalled(p);
      card.className = `participant-card agent-card ${actor}${stalled ? ' stalled' : ''}`;
      card.dataset.actor = actor;

      const avatar = document.createElement('div');
      avatar.className = `avatar avatar-${actor}`;
      avatar.textContent = (p.display_name || displayName(actor)).slice(0, 1).toUpperCase();
      card.appendChild(avatar);

      const main = document.createElement('div');
      main.className = 'participant-main';
      const title = document.createElement('div');
      title.className = 'participant-title';
      const strong = document.createElement('strong');
      strong.textContent = p.display_name || displayName(actor);
      const roleBadge = document.createElement('span');
      roleBadge.className = 'role-badge';
      roleBadge.textContent = responsibilityText(p.responsibility);
      title.append(strong, roleBadge);
      main.appendChild(title);

      const subtitle = document.createElement('div');
      subtitle.className = 'participant-subtitle';
      subtitle.textContent = p.last_error || [p.mention_handle, sessionSummary(p)].filter(Boolean).join(' · ');
      main.appendChild(subtitle);
	  const mentionButton = document.querySelector(`.target-button[data-target="${actor}"]`);
	  if (mentionButton) mentionButton.textContent = p.mention_handle || `@${actor}`;
	  const inspectorOption = document.querySelector(`#inspector-agent option[value="${actor}"]`);
	  if (inspectorOption) inspectorOption.textContent = p.display_name || displayName(actor);

      const copy = document.createElement('button');
      copy.type = 'button';
      copy.className = 'ghost-button compact-button session-copy';
      const vendorID = String(p.session_id || '').trim();
	  const runtimeKind = p.runtime_kind || p.runtime?.runtime_kind || (actor === 'claude' ? 'claude' : 'codex');
      if (!vendorID) {
        copy.disabled = true;
        copy.textContent = t("ui.notYetCreated");
        copy.title = t("ui.theNativeSessionThreadIdIsCreatedAfterTheFirstAcceptedTurn");
      } else {
        copy.textContent = runtimeKind === 'codex' ? t("ui.copyThreadId") : t("ui.copySessionId");
        copy.title = t("ui.copyTheFullIdToResumeTheNativeSession");
        copy.addEventListener('click', async () => {
          try {
            await navigator.clipboard.writeText(vendorID);
            toast(t("ui.copiedFullId"), 'success');
          } catch {
            toast(t("ui.copyFailed"), 'error');
          }
        });
      }
      main.appendChild(copy);

      const meta = document.createElement('div');
      meta.className = 'participant-meta';
      const status = document.createElement('span');
      status.className = `state-badge state-${p.state}`;
      status.textContent = stateText(p.state);
      const model = document.createElement('span');
      model.className = 'participant-subtitle';
      model.textContent = [p.runtime?.provider, p.model || t('room.nativeDefault')].filter(Boolean).join(' · ');
      meta.append(status, model);
      main.appendChild(meta);

      const runtime = p.runtime || {};
      if (runtime.session_name) {
        const nameLine = document.createElement('button');
        nameLine.type = 'button';
        nameLine.className = 'ghost-button runtime-session-name';
        nameLine.textContent = runtime.session_name;
        nameLine.title = t('room.copyRuntimeName') + ': ' + runtime.session_name;
        nameLine.addEventListener('click', async () => {
          try { await navigator.clipboard.writeText(runtime.session_name); toast(t('room.runtimeNameCopied'), 'success'); }
          catch { toast(t('ui.copyFailed'), 'error'); }
        });
        main.appendChild(nameLine);
        const nameStatus = document.createElement('div');
        nameStatus.className = 'participant-subtitle runtime-name-status';
        const statuses = {
          pending: 'room.runtimeNamePending', configured: 'room.runtimeNameConfigured', synced: 'room.runtimeNameSynced',
          unsupported: 'room.runtimeNameUnsupported', failed: 'room.runtimeNameFailed', simulated: 'room.runtimeNameSimulated',
        };
        nameStatus.textContent = t(statuses[runtime.session_name_status] || 'room.runtimeNamePending');
        main.appendChild(nameStatus);
      }
      if (runtime.protocol || runtime.version || runtime.path || runtime.command) {
        const runtimeLine = document.createElement('div');
        runtimeLine.className = 'runtime-line';
        const pieces = [runtime.version ? `v${runtime.version}` : '', runtime.protocol, runtime.available === false ? t('agent.unavailable') : ''].filter(Boolean);
        runtimeLine.textContent = pieces.join(' · ') || runtime.command || t('room.runtimeDetected');
        runtimeLine.title = [runtime.path, ...(runtime.capabilities || [])].filter(Boolean).join('\n');
        main.appendChild(runtimeLine);
      }
      if (stalled) {
        const warning = document.createElement('div');
        warning.className = 'runtime-line runtime-warning';
        warning.textContent = t("ui.noObservableEventsForAWhileSilenceIsNotAStallBy");
        main.appendChild(warning);
      }
      if (runtime.warnings && runtime.warnings.length) {
        const warning = document.createElement('div');
        warning.className = 'runtime-line runtime-warning';
        warning.textContent = truncate(runtime.warnings.join(' · '), 160);
        warning.title = runtime.warnings.join('\n');
        main.appendChild(warning);
      }

      const policy = participantPolicy(p);
      const policyLine = document.createElement('div');
      policyLine.className = `native-policy ${policy.protected ? 'protected' : ''}`;
      policyLine.textContent = policy.text;
      policyLine.title = policy.title;
      main.appendChild(policyLine);

	  const workspace = p.workspace || {};
	  if (workspace.kind) {
		const workspaceLine = document.createElement('div');
		workspaceLine.className = `workspace-boundary ${workspace.read_only ? 'protected' : ''}`;
		const parts = [workspace.kind === 'reviewer-snapshot' ? t("ui.independentReviewSnapshot") : t("ui.liveWorkspace")];
		if (workspace.dirty) parts.push(t("ui.hasUncommittedChanges"));
		if (workspace.untracked_count) parts.push(t("ui.valueUntrackedFiles", { value0: (workspace.untracked_count) }));
		workspaceLine.textContent = parts.join(' · ');
		workspaceLine.title = [
		  workspace.path,
		  workspace.source_head ? `HEAD ${workspace.source_head}` : '',
		  workspace.patch_sha256 ? `snapshot ${workspace.patch_sha256}` : '',
		  ...(workspace.warnings || []),
		].filter(Boolean).join('\n');
		main.appendChild(workspaceLine);
	  }

      if (state.snapshot.meta.collaboration) {
        const permission = document.createElement('select');
        permission.className = 'permission-select';
        permission.dataset.permissionActor = actor;
        permission.setAttribute('aria-label', `${displayName(actor)} · ${t('room.collaboration.permissions')}`);
        for (const [value, key] of [['configured','room.collaboration.configured'],['read-only','room.collaboration.readOnly'],['yolo','room.collaboration.yolo']]) {
          const option = document.createElement('option'); option.value = value; option.textContent = t(key);
          option.selected = (p.permission_profile || 'configured') === value; permission.appendChild(option);
        }
        permission.disabled = participantBusy(actor) || Object.values(state.snapshot.participants).some((p) => !['stopped','idle','error'].includes(p.state));
        main.appendChild(permission);
      }

      const actions = document.createElement('div');
      actions.className = 'agent-actions';
      if (p.state === 'stopped' || p.state === 'error') {
        actions.appendChild(actionButton(actor, 'start', t("ui.start")));
      } else {
        actions.appendChild(actionButton(actor, 'interrupt', t("ui.interrupt")));
        actions.appendChild(actionButton(actor, 'restart', t("ui.restart")));
        actions.appendChild(actionButton(actor, 'stop', t("ui.stop"), true));
      }
      main.appendChild(actions);
      card.appendChild(main);
      container.appendChild(card);
    });
  }

  function actionButton(actor, action, label, danger = false) {
    const button = document.createElement('button');
    button.className = `mini-button${danger ? ' danger' : ''}`;
    button.dataset.actor = actor;
    button.dataset.action = action;
    button.textContent = label;
    button.disabled = participantBusy(actor);
    button.setAttribute('aria-busy', String(state.participantActions.has(actor)));
    return button;
  }

  function renderSettings() {
    if (!state.settingsDirty && !state.settingsSaving) {
      const stall = Number(state.snapshot?.settings?.stall_warning_seconds ?? 300);
      $('stall-disabled').checked = stall < 0;
      $('stall-warning').value = stall < 0 ? 300 : stall;
    }
    $('stall-warning').disabled = $('stall-disabled').checked;
    const save = $('save-settings');
    save.removeAttribute('data-i18n');
    save.disabled = state.settingsSaving || !state.snapshot;
    save.setAttribute('aria-busy', String(state.settingsSaving));
    save.textContent = t(state.settingsSaving ? 'room.savingSettings' : 'ui.saveSettings');
    $('settings-status').textContent = state.settingsDirty ? t('room.unsavedSettings') : '';
  }

  function editSettings() {
    state.settingsRevision += 1;
    state.settingsDirty = true;
    renderSettings();
  }

  function renderStreamingDrafts() {
    if (!state.snapshot || !timeline.isConnected) return;
    const nearBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 140;
    let hasVisibleDraft = false;
    ['claude', 'codex'].forEach((actor) => {
      const selector = `.message-row.streaming[data-streaming-actor="${actor}"]`;
      const existing = timeline.querySelector(selector);
      const text = state.drafts[actor] || '';
      const correlation = state.draftCorrelation[actor] || '';
      const correlated = (state.snapshot.messages || []).find((message) => message.id === correlation);
      const threadVisible = !state.threadFilter || correlated?.thread_id === state.threadFilter;
      const visible = Boolean(text) && threadVisible && state.conversationFilter !== 'human';
      if (!visible) {
        if (existing) existing.remove();
        return;
      }
      let row = existing;
      if (!row) {
        row = draftNode(actor, text, correlation);
        timeline.appendChild(row);
      } else {
        row.dataset.streamingCorrelation = correlation;
        const streamingText = row.querySelector('[data-streaming-text]');
        if (streamingText && streamingText.textContent !== text) streamingText.textContent = text;
      }
      hasVisibleDraft = true;
    });
    if (hasVisibleDraft) {
      const empty = timeline.querySelector('.timeline-empty');
      if (empty) empty.remove();
    }
    if (nearBottom) requestAnimationFrame(scrollBottom);
  }

  function renderMessage(messageID) {
    if (!messageID || !state.snapshot || !timeline.isConnected) return;
    const existing = Array.from(timeline.querySelectorAll('.message-row[data-message-id]'))
      .find((row) => row.dataset.messageId === messageID);
    if (!existing) return;
    const message = (state.snapshot.messages || []).find((item) => item.id === messageID);
    if (!message) {
      existing.remove();
      return;
    }
    const currentDelivery = existing.querySelector('.delivery-line');
    const nextDelivery = deliveryNode(message);
    if (currentDelivery && nextDelivery) currentDelivery.replaceWith(nextDelivery);
    else if (currentDelivery) currentDelivery.remove();
    else if (nextDelivery) existing.querySelector('.message-content')?.appendChild(nextDelivery);
  }

  function renderCreatedMessage(messageID) {
    if (!messageID || !state.snapshot || !timeline.isConnected) return;
    const message = (state.snapshot.messages || []).find((item) => item.id === messageID);
    if (!message) return;
    const existing = Array.from(timeline.querySelectorAll('.message-row[data-message-id]'))
      .find((row) => row.dataset.messageId === messageID);
    if (existing) {
      renderMessage(messageID);
      return;
    }
    const streaming = Array.from(timeline.querySelectorAll('.message-row.streaming'))
      .find((row) => row.dataset.streamingActor === message.from
        && (!message.reply_to || row.dataset.streamingCorrelation === message.reply_to));
    if (state.searchQuery.trim() || state.threadFilter || state.conversationFilter !== 'all') {
      if (streaming) streaming.remove();
      queueRender();
      return;
    }

    const nearBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 140;
    const empty = timeline.querySelector('.timeline-empty');
    if (empty) empty.remove();
    const firstStreaming = timeline.querySelector('.message-row.streaming');
    const separators = timeline.querySelectorAll('.timeline-date');
    const lastDateKey = separators.length ? separators[separators.length - 1].dataset.dateKey || '' : '';
    const dateKey = localDateKey(message.created_at);
    if (dateKey && dateKey !== lastDateKey) {
      const separator = dateSeparatorNode(message.created_at);
      if (firstStreaming) timeline.insertBefore(separator, firstStreaming);
      else timeline.appendChild(separator);
    }
    const node = messageNode(message);
    node.classList.add('is-new');
    if (firstStreaming) timeline.insertBefore(node, firstStreaming);
    else timeline.appendChild(node);
    if (streaming) streaming.remove();
    renderTimelineScope();
    if (nearBottom) requestAnimationFrame(scrollBottom);
  }

  function renderTimeline() {
    timeline.replaceChildren();
    renderTimelineScope();
    const items = [];
    for (const message of state.snapshot.messages || []) {
      items.push({ seq: message.seq, type: 'message', value: message, createdAt: message.created_at });
    }
    for (const event of state.snapshot.events || []) {
      if (event.kind === 'system.notice') {
        items.push({ seq: event.seq, type: 'notice', value: event.data, createdAt: event.created_at });
      }
    }
    items.sort((a, b) => a.seq - b.seq);

    const windowInfo = state.snapshot.message_window;
    if (windowInfo?.has_more && !state.threadFilter) {
      const older = document.createElement('button');
      older.type = 'button';
      older.className = 'load-older-button';
      older.dataset.loadOlder = 'true';
      older.disabled = state.loadingOlder;
      older.textContent = state.loadingOlder
        ? t("ui.loadingEarlierMessages")
        : t("ui.loadEarlierMessagesShowingValueValue", { value0: (windowInfo.loaded || state.snapshot.messages.length), value1: (windowInfo.total) });
      timeline.appendChild(older);
    }

    const query = state.searchQuery.trim().toLocaleLowerCase();
    let visibleCount = 0;
    let lastDateKey = '';
    const appendVisible = (node, createdAt) => {
      const key = localDateKey(createdAt);
      if (key && key !== lastDateKey) {
        timeline.appendChild(dateSeparatorNode(createdAt));
        lastDateKey = key;
      }
      timeline.appendChild(node);
      visibleCount += 1;
    };

    for (const item of items) {
      if (item.type === 'notice') {
        const visible = !state.threadFilter && (!query || String(item.value.text || '').toLocaleLowerCase().includes(query));
        if (visible) appendVisible(noticeNode(item.value), item.createdAt);
        continue;
      }
      const filterVisible = state.conversationFilter === 'all'
        || (state.conversationFilter === 'agents' && ['claude', 'codex'].includes(item.value.from))
        || (state.conversationFilter === 'human' && item.value.from === 'user');
      const threadVisible = !state.threadFilter || item.value.thread_id === state.threadFilter;
      const attachmentText = (item.value.attachments || []).map((attachment) => attachment.name).join(' ');
      const searchVisible = !query || `${displayName(item.value.from)} ${item.value.text} ${attachmentText}`.toLocaleLowerCase().includes(query);
      if (threadVisible && filterVisible && searchVisible) appendVisible(messageNode(item.value), item.createdAt);
    }
    ['claude', 'codex'].forEach((actor) => {
      const text = state.drafts[actor];
      const correlated = (state.snapshot.messages || []).find((message) => message.id === state.draftCorrelation[actor]);
      const threadVisible = !state.threadFilter || correlated?.thread_id === state.threadFilter;
      if (text && threadVisible && state.conversationFilter !== 'human') {
        timeline.appendChild(draftNode(actor, text, state.draftCorrelation[actor]));
        visibleCount += 1;
      }
    });
    if (!(state.snapshot.messages || []).length && !state.drafts.claude && !state.drafts.codex
      && !windowInfo?.has_more && !query && !state.threadFilter && state.conversationFilter === 'all') {
      const empty = document.createElement('div');
      empty.className = 'timeline-empty';
      const inner = document.createElement('div');
      inner.innerHTML = "<div class=\"empty-orbit\"></div><h2 data-i18n=\"ui.startAThreePartyCollaboration\">Start a three-party collaboration</h2><p data-i18n=\"room.startWithOneAgent\">Choose a starting Agent and describe your goal. Only an exact peer mention continues the relay; you stay in control.</p>";
      empty.appendChild(inner);
      timeline.appendChild(empty);
    }

    if ((query || state.conversationFilter !== 'all' || state.threadFilter) && visibleCount === 0) {
      const empty = document.createElement('div');
      empty.className = 'timeline-empty';
	  const title = document.createElement('h2');
	  title.textContent = t('room.noMatchingMessages');
	  const detail = document.createElement('p');
	  detail.textContent = t('room.restoreTimelineFilter');
	  empty.append(title, detail);
      timeline.appendChild(empty);
    }
  }

  function renderTimelineScope() {
    const banner = $('timeline-scope');
    if (!state.threadFilter) {
      banner.classList.add('hidden');
      $('timeline-scope-text').textContent = '';
      return;
    }
    const messages = (state.snapshot?.messages || []).filter((message) => message.thread_id === state.threadFilter);
    const root = messages[0];
    const summary = root ? truncate(root.text || attachmentSummary(root), 100) : state.threadFilter;
    $('timeline-scope-text').textContent = t("ui.valueMessagesValue", { value0: (messages.length), value1: (summary) });
    banner.classList.remove('hidden');
  }

  function focusThread(threadId) {
    if (!threadId) return;
    state.threadFilter = threadId;
    timeline.scrollTop = 0;
    queueRender();
  }

  function clearThreadFilter() {
    state.threadFilter = '';
    timeline.scrollTop = 0;
    queueRender();
  }

  function localDateKey(value) {
    const date = new Date(value || '');
    if (Number.isNaN(date.getTime())) return '';
    const year = date.getFullYear();
    const month = String(date.getMonth() + 1).padStart(2, '0');
    const day = String(date.getDate()).padStart(2, '0');
    return `${year}-${month}-${day}`;
  }

  function dateSeparatorNode(value) {
    const separator = document.createElement('div');
    separator.className = 'timeline-date';
    separator.dataset.dateKey = localDateKey(value);
    const date = new Date(value || '');
    const today = new Date();
    const yesterday = new Date(today);
    yesterday.setDate(today.getDate() - 1);
    const key = localDateKey(date);
    let label = '';
    if (key === localDateKey(today)) label = t("ui.today");
    else if (key === localDateKey(yesterday)) label = t("ui.yesterday");
    else {
      label = window.PairRoomI18n.formatDate(date, {
        year: date.getFullYear() === today.getFullYear() ? undefined : 'numeric',
        month: 'long', day: 'numeric', weekday: 'short',
      });
    }
    const span = document.createElement('span');
    span.textContent = label;
    separator.appendChild(span);
    return separator;
  }

  function messageNode(message) {
    const actor = message.from;
    const row = document.createElement('article');
    row.className = `message-row ${actor}`;
    row.dataset.messageId = message.id;

    const avatar = document.createElement('div');
    avatar.className = `message-avatar avatar-${actor === 'user' ? 'human' : actor}`;
    avatar.textContent = actor === 'user' ? 'Y' : actor === 'claude' ? '1' : '2';

    const content = document.createElement('div');
    content.className = 'message-content';
    const meta = document.createElement('div');
    meta.className = 'message-meta';
    const author = document.createElement('span');
    author.className = 'message-author';
    author.textContent = displayName(actor);
    const time = document.createElement('time');
    time.textContent = formatTime(message.created_at);
    time.dateTime = message.created_at || '';
    const actions = document.createElement('span');
    actions.className = 'message-actions';
    const inspect = document.createElement('button');
    inspect.className = 'reply-action inspect-action';
    inspect.textContent = t("ui.activity");
    inspect.dataset.inspectId = message.id;
    const copy = document.createElement('button');
    copy.className = 'reply-action';
    copy.textContent = t("ui.copy");
    copy.dataset.copyMessage = message.id;
    const thread = document.createElement('button');
    thread.className = 'reply-action';
    thread.textContent = t("ui.thread");
    thread.dataset.threadId = message.thread_id || '';
    thread.title = t("ui.viewOnlyThisThread");
    const reply = document.createElement('button');
    reply.className = 'reply-action';
    reply.textContent = t("ui.reply");
    reply.dataset.replyId = message.id;
    actions.append(inspect, copy, thread, reply);
    meta.append(author, time);
    if (message.retry_of) {
      const retryMarker = document.createElement('span');
      retryMarker.className = 'retry-marker';
      retryMarker.textContent = t("ui.retry");
      retryMarker.title = t('room.retryOfValue', { value: message.retry_of });
      meta.appendChild(retryMarker);
    }
	if (message.intent && message.intent !== 'steer') {
	  const intentMarker = document.createElement('span');
	  intentMarker.className = `intent-marker intent-${message.intent}`;
	  intentMarker.textContent = t("ui.queued");
	  meta.appendChild(intentMarker);
	}
    meta.appendChild(actions);

    const bubble = document.createElement('div');
    bubble.className = 'message-bubble';
    const body = document.createElement('div');
    body.className = 'message-body';
    if (message.reply_to) {
      const parent = state.snapshot.messages.find((item) => item.id === message.reply_to);
      if (parent) {
        const quote = document.createElement('button');
        quote.type = 'button';
        quote.className = 'reply-quote';
        quote.dataset.scrollMessage = parent.id;
        const parentSummary = parent.text || window.PairRoomI18n.formatList((parent.attachments || []).map((item) => item.name)) || t("ui.imageMessage");
        quote.textContent = `${displayName(parent.from)}：${truncate(parentSummary, 110)}`;
        body.appendChild(quote);
      }
    }
    const usedAttachments = appendRichContent(body, message.text, { message });
    appendAttachmentGallery(body, message.attachments || [], usedAttachments);
    bubble.appendChild(body);

    const isLongMessage = String(message.text || '').length > 3200
      || String(message.text || '').split('\n').length > 85
      || (message.attachments || []).length > 6;
    const isExpanded = state.expandedMessages.has(message.id);
    if (isLongMessage) {
      if (!isExpanded) body.classList.add('long-message-fade');
      const expand = document.createElement('button');
      expand.type = 'button';
      expand.className = 'expand-message';
      expand.dataset.expandMessage = message.id;
      expand.textContent = isExpanded ? t("ui.collapse") : t("ui.expandMessage");
      expand.setAttribute('aria-expanded', String(isExpanded));
      bubble.appendChild(expand);
    }
    content.append(meta, bubble);

    const delivery = deliveryNode(message);
    if (delivery) content.appendChild(delivery);
    row.append(avatar, content);
    return row;
  }

  function deliveryNode(message) {
    if (!message.delivery || Object.keys(message.delivery).length === 0) return null;
    const delivery = document.createElement('div');
    delivery.className = 'delivery-line';
    for (const target of Object.keys(message.delivery)) {
      const chip = document.createElement('span');
      const status = message.delivery[target];
      const processing = message.processing && message.processing[target];
      chip.className = `delivery-chip ${status} processing-${processing || 'waiting'}`;
      chip.textContent = `${displayName(target)} · ${deliveryText(status)}${processing ? ` / ${processingText(processing)}` : ''}`;
      const deliveryDetail = message.delivery_detail && message.delivery_detail[target];
      const processingDetail = message.processing_detail && message.processing_detail[target];
      const turn = message.processing_turn && message.processing_turn[target];
      chip.title = [deliveryDetail, processingDetail, turn ? t('room.turnValue', { value: turn }) : ''].filter(Boolean).join('\n');
      delivery.appendChild(chip);
      if (isRetryable(message, target)) {
        const retryButton = document.createElement('button');
        retryButton.className = 'retry-button';
        retryButton.dataset.retryId = message.id;
        retryButton.dataset.retryTarget = target;
        retryButton.disabled = state.messageActions.has(messageActionKey(message.id, target)) || pendingRetry(message.id, target);
        retryButton.setAttribute('aria-busy', String(state.messageActions.has(messageActionKey(message.id, target))));
        if (pendingRetry(message.id, target)) retryButton.title = t('room.retryPending');
        retryButton.textContent = t("ui.retryValue", { value0: (displayName(target)) });
        delivery.appendChild(retryButton);
      }
      if (processing === 'waiting' || processing === 'working') {
        const cancelButton = document.createElement('button');
        cancelButton.className = 'cancel-message-button';
        cancelButton.dataset.cancelMessage = message.id;
        cancelButton.dataset.cancelTarget = target;
        cancelButton.disabled = state.messageActions.has(messageActionKey(message.id, target));
        cancelButton.setAttribute('aria-busy', String(cancelButton.disabled));
        cancelButton.textContent = t("ui.cancelValue", { value0: (displayName(target)) });
        cancelButton.title = status === 'pending'
          ? t("ui.thisMessageIsStillInTheRoomFifoRemovingItWillNot")
          : t("ui.thisInputIsAlreadyInTheNativeRuntimeCancellingMayInterruptThis");
        delivery.appendChild(cancelButton);
      }
    }
    return delivery;
  }

  function draftNode(actor, text, correlation) {
    const row = document.createElement('article');
    row.className = `message-row ${actor} streaming`;
    row.dataset.streamingActor = actor;
    row.dataset.streamingCorrelation = correlation || '';
    const avatar = document.createElement('div');
    avatar.className = `message-avatar avatar-${actor}`;
    avatar.textContent = actor === 'claude' ? '1' : '2';
    const content = document.createElement('div');
    content.className = 'message-content';
    const meta = document.createElement('div');
    meta.className = 'message-meta';
    const author = document.createElement('span');
    author.className = 'message-author';
    author.textContent = displayName(actor);
    const typing = document.createElement('span');
    typing.textContent = t("ui.typing");
    meta.append(author, typing);
    const bubble = document.createElement('div');
    bubble.className = 'message-bubble';
    const streamingText = document.createElement('span');
    streamingText.dataset.streamingText = 'true';
    streamingText.textContent = text;
    bubble.appendChild(streamingText);
    const caret = document.createElement('span');
    caret.className = 'typing-caret';
    bubble.appendChild(caret);
    content.append(meta, bubble);
    row.append(avatar, content);
    return row;
  }

  function noticeNode(notice) {
    const row = document.createElement('div');
    row.className = 'system-message';
    const chip = document.createElement('div');
    chip.className = `system-chip ${notice.level || 'info'}`;
    chip.textContent = notice.text || t('room.pairroomEvent');
    row.appendChild(chip);
    return row;
  }

  function appendRichContent(parent, text, options = {}) {
    const attachments = options.message ? (options.message.attachments || []) : [];
    const used = new Set();
    if (!String(text || '').trim()) return used;
    const renderer = window.PairRoomRichText;
    if (!renderer || typeof renderer.render !== 'function') {
      const fallback = document.createElement('div');
      fallback.className = 'rich-content';
      fallback.textContent = String(text || '');
      parent.appendChild(fallback);
      return used;
    }
    renderer.render(parent, text, {
      onCopyError: () => toast(t("ui.theBrowserBlockedClipboardWrites"), 'error'),
      createImage: (reference, alt, title) => createMarkdownImage(reference, alt, title, attachments, used),
    });
    return used;
  }

  function createMarkdownImage(reference, alt, title, attachments, used) {
    const match = matchAttachment(reference, attachments, used);
    if (match) {
      used.add(match.id);
      return createAttachmentCard(match, { inline: true, alt: alt || match.name, title });
    }
    const value = String(reference || '').trim();
    const placeholder = document.createElement('div');
    placeholder.className = /^https?:\/\//i.test(value) ? 'external-image-placeholder' : 'external-image-placeholder missing-local-image';
    const label = document.createElement('span');
    label.textContent = /^https?:\/\//i.test(value)
      ? t("ui.externalImageWasNotLoadedAutomaticallyValue", { value0: (alt || value) })
      : t("ui.noSafelyPreviewableLocalImageWasFoundValue", { value0: (alt || value) });
    placeholder.appendChild(label);
    if (/^https?:\/\//i.test(value)) {
      const link = document.createElement('a');
      link.href = value;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.textContent = t("ui.openLink");
      placeholder.appendChild(link);
    }
    return placeholder;
  }

  function matchAttachment(reference, attachments, used) {
    let decoded = String(reference || '').trim().replace(/^file:\/\//i, '');
    try { decoded = decodeURIComponent(decoded); } catch { /* preserve original */ }
    decoded = decoded.replace(/\\/g, '/').replace(/[?#].*$/, '');
    const basename = decoded.split('/').filter(Boolean).pop() || decoded;
    return attachments.find((item) => !used.has(item.id)
      && (item.id === decoded || item.name === decoded || item.name === basename)) || null;
  }

  function appendAttachmentGallery(parent, attachments, used = new Set()) {
    const remaining = (attachments || []).filter((value) => !used.has(value.id));
    if (!remaining.length) return;
    const gallery = document.createElement('div');
    gallery.className = `message-attachments${remaining.length === 1 ? ' single' : ''}`;
    remaining.forEach((value) => gallery.appendChild(createAttachmentCard(value)));
    parent.appendChild(gallery);
  }

  function createAttachmentCard(attachment, options = {}) {
    const inline = Boolean(options.inline);
    const card = document.createElement(inline ? 'figure' : 'div');
    card.className = inline ? 'inline-markdown-image' : 'message-image-card';
    const stage = document.createElement('button');
    stage.type = 'button';
    stage.className = inline ? 'message-image-stage inline-image-stage' : 'message-image-stage';
    stage.title = t("ui.previewValue", { value0: (attachment.name) });
    stage.dataset.openAttachment = attachment.id;
    const loading = document.createElement('span');
    loading.className = 'image-loading';
    loading.textContent = t("ui.loadingImage");
    stage.appendChild(loading);
    const caption = document.createElement(inline ? 'figcaption' : 'div');
    caption.className = inline ? 'inline-image-caption' : 'message-image-caption';
    const name = document.createElement('span');
    name.textContent = options.title || options.alt || attachment.name;
    name.title = attachment.name;
    const meta = document.createElement('span');
    meta.textContent = imageMeta(attachment);
    caption.append(name, meta);
    card.append(stage, caption);

    loadAttachmentURL(attachment).then((url) => {
      if (!stage.isConnected && !card.isConnected) return;
      stage.replaceChildren();
      const image = document.createElement('img');
      image.src = url;
      image.alt = options.alt || attachment.name;
      image.loading = 'lazy';
      image.decoding = 'async';
      stage.appendChild(image);
    }).catch((error) => {
      stage.replaceChildren();
      const failed = document.createElement('span');
      failed.className = 'image-loading image-error';
      failed.textContent = t("ui.imageFailedToLoadValue", { value0: (error.message) });
      stage.appendChild(failed);
    });
    return card;
  }

  async function loadAttachmentURL(attachment) {
    const existing = state.mediaObjectURLs.get(attachment.id);
    if (typeof existing === 'string') return existing;
    if (existing && typeof existing.then === 'function') return existing;
    const pending = apiBlob(`/api/v1/attachments/${encodeURIComponent(attachment.id)}`)
      .then((blob) => {
        const url = URL.createObjectURL(blob);
        state.mediaObjectURLs.set(attachment.id, url);
        return url;
      })
      .catch((error) => {
        state.mediaObjectURLs.delete(attachment.id);
        throw error;
      });
    state.mediaObjectURLs.set(attachment.id, pending);
    return pending;
  }

  function lightboxGroup(attachment) {
    const message = (state.snapshot?.messages || []).find((item) => (item.attachments || []).some((value) => value.id === attachment.id));
    const values = message ? (message.attachments || []) : [attachment];
    const seen = new Set();
    return values.filter((value) => value?.id && !seen.has(value.id) && seen.add(value.id));
  }

  function openLightbox(attachment, url) {
    state.lightboxItems = lightboxGroup(attachment);
    state.lightboxIndex = Math.max(0, state.lightboxItems.findIndex((value) => value.id === attachment.id));
    const dialog = $('image-lightbox');
    if (!dialog.open) dialog.showModal();
    void showLightboxAttachment(url);
  }

  async function showLightboxAttachment(knownURL = '') {
    const attachment = state.lightboxItems[state.lightboxIndex];
    if (!attachment) return;
    const request = ++state.lightboxRequest;
    const image = $('lightbox-image');
    const stage = $('lightbox-stage');
    stage.classList.add('loading');
    image.removeAttribute('src');
    image.alt = attachment.name;
    $('lightbox-title').textContent = attachment.name;
    $('lightbox-meta').textContent = `${attachment.media_type || 'image'} · ${imageMeta(attachment)}`;
    $('lightbox-counter').textContent = state.lightboxItems.length > 1
      ? `${state.lightboxIndex + 1} / ${state.lightboxItems.length}` : '';
    $('lightbox-prev').disabled = state.lightboxItems.length < 2;
    $('lightbox-next').disabled = state.lightboxItems.length < 2;
    state.lightboxRotation = 0;
    state.lightboxMode = 'fit';
    setLightboxZoom(1);
    updateLightboxMode();
    try {
      const url = knownURL || await loadAttachmentURL(attachment);
      if (request !== state.lightboxRequest) return;
      image.src = url;
      $('lightbox-open').href = url;
      $('lightbox-open').download = attachment.name || 'pairroom-image';
    } catch (error) {
      if (request !== state.lightboxRequest) return;
      $('lightbox-title').textContent = t("ui.imageFailedToLoadValue", { value0: (attachment.name) });
      $('lightbox-meta').textContent = error.message;
      $('lightbox-open').href = '#';
    } finally {
      if (request === state.lightboxRequest) stage.classList.remove('loading');
    }
  }

  function moveLightbox(delta) {
    if (state.lightboxItems.length < 2) return;
    state.lightboxIndex = (state.lightboxIndex + delta + state.lightboxItems.length) % state.lightboxItems.length;
    void showLightboxAttachment();
  }

  function setLightboxZoom(value) {
    state.lightboxZoom = Math.min(8, Math.max(0.25, Number(value) || 1));
    applyLightboxTransform();
  }

  function rotateLightbox(delta) {
    state.lightboxRotation = (state.lightboxRotation + delta + 360) % 360;
    applyLightboxTransform();
  }

  function setLightboxMode(mode) {
    state.lightboxMode = mode === 'actual' ? 'actual' : 'fit';
    state.lightboxZoom = 1;
    updateLightboxMode();
    applyLightboxTransform();
  }

  function updateLightboxMode() {
    const image = $('lightbox-image');
    image.classList.toggle('actual-size', state.lightboxMode === 'actual');
    const fit = $('lightbox-fit');
    const actual = $('lightbox-actual');
    fit.classList.toggle('active', state.lightboxMode === 'fit');
    actual.classList.toggle('active', state.lightboxMode === 'actual');
    fit.setAttribute('aria-pressed', String(state.lightboxMode === 'fit'));
    actual.setAttribute('aria-pressed', String(state.lightboxMode === 'actual'));
  }

  function applyLightboxTransform() {
    $('lightbox-image').style.transform = `rotate(${state.lightboxRotation}deg) scale(${state.lightboxZoom})`;
    $('lightbox-zoom-value').textContent = `${Math.round(state.lightboxZoom * 100)}%`;
    $('lightbox-stage').classList.toggle('zoomed', state.lightboxZoom > 1 || state.lightboxMode === 'actual');
  }

  async function copyLightboxImage() {
    const attachment = state.lightboxItems[state.lightboxIndex];
    if (!attachment || !navigator.clipboard || typeof ClipboardItem === 'undefined') {
      toast(t("ui.thisBrowserCannotCopyImages"), 'error');
      return;
    }
    try {
      const blob = await apiBlob(`/api/v1/attachments/${encodeURIComponent(attachment.id)}`);
      const type = blob.type || attachment.media_type || 'image/png';
      await navigator.clipboard.write([new ClipboardItem({ [type]: blob })]);
      toast(t("ui.imageCopiedToClipboard"), 'success');
    } catch (error) {
      toast(t("ui.couldNotCopyImageValue", { value0: (error.message) }), 'error');
    }
  }

  function imageMeta(value) {
    const dimensions = value.width && value.height ? `${value.width}×${value.height}` : '';
    return [dimensions, formatBytes(value.size)].filter(Boolean).join(' · ');
  }

  let activityView;
  function renderActivity() {
    if (!state.snapshot || state.inspectorTab !== 'activity' || document.hidden || !state.shellActive) return;
    if (document.querySelector('.inspector-panel')?.getAttribute('aria-hidden') === 'true') return;
    const scope = $('inspector-scope');
    const scopedMessage = state.inspectorCorrelation
      ? (state.snapshot.messages || []).find((message) => message.id === state.inspectorCorrelation) : null;
    const turnIDs = new Set(scopedMessage ? Object.values(scopedMessage.processing_turn || {}).filter(Boolean) : []);
    scope.classList.toggle('hidden', !scopedMessage);
    $('inspector-scope-text').textContent = scopedMessage
      ? t('ui.showOnlyWorkForValueMessageValue', { value0: displayName(scopedMessage.from), value1: truncate(scopedMessage.id, 18) }) : '';
    const summaries = (state.snapshot.turns || [])
      .filter((turn) => state.inspectorAgent === 'all' || turn.agent === state.inspectorAgent)
      .filter((turn) => !scopedMessage || (turn.message_ids || []).includes(scopedMessage.id) || turnIDs.has(turn.turn_id))
      .sort((a, b) => String(b.updated_at || b.started_at).localeCompare(String(a.updated_at || a.started_at)))
      .slice(0, 40);
    const events = (state.snapshot.events || [])
      .filter((event) => event.kind === 'runtime.event')
      .filter((event) => state.inspectorAgent === 'all' || event.data?.agent === state.inspectorAgent)
      .filter((event) => !['text.delta', 'state', 'session'].includes(event.data?.kind))
      .filter((event) => !scopedMessage || event.data?.correlation_id === scopedMessage.id || turnIDs.has(event.data?.turn_id))
      .slice(-100).reverse();
    activityView ||= window.PairRoomActivity.create($('activity-tab'), {
      t, displayName, formatTime, formatDuration, turnStatusText, activityIcon, activityLabel, activityDetail, prettyJSON, truncate,
    });
    activityView.render(summaries, events, {
      scoped: Boolean(scopedMessage),
      version: JSON.stringify([window.PairRoomI18n?.lang, displayName('claude'), displayName('codex')]),
      emptyText: t(scopedMessage ? 'ui.thisMessageDoesNotYetHaveADurableWorkSummary' : 'ui.agentTurnsToolCallsCommandsPlansDiffsAndLogsAppearHere'),
    });
  }

  function turnStatusText(status) {
    return ({ working: t('ui.working694b71b'), waiting: t('ui.waiting'), completed: t('common.done'), cancelled: t('ui.cancelled'), failed: t('ui.failed') })[status] || (status || t('common.unknown'));
  }

  function formatDuration(milliseconds) {
    const value = Math.max(0, Number(milliseconds) || 0);
	const format = (number, maximumFractionDigits = 0) => window.PairRoomI18n.formatNumber(number, { maximumFractionDigits });
    if (value < 1000) return `${format(Math.round(value))} ms`;
    if (value < 60_000) return `${format(value / 1000, value < 10_000 ? 1 : 0)} s`;
    const minutes = Math.floor(value / 60_000);
    const seconds = Math.round((value % 60_000) / 1000);
	return `${format(minutes)}m ${format(seconds)}s`;
  }

  function renderApprovals() {
    const container = $('approvals-tab');
    const pending = (state.snapshot.approvals || []).filter((item) => item.status === 'pending');
    const ids = new Set(pending.map((item) => item.id));
    for (const id of state.approvalSubmissions.keys()) {
      if (!ids.has(id)) state.approvalSubmissions.delete(id);
    }
    $('approval-count').textContent = String(pending.length);
    if (!pending.length) {
      if (!container.querySelector('.approvals-empty')) {
        const empty = document.createElement('div');
        empty.className = 'approvals-empty';
        container.replaceChildren(empty);
      }
      container.firstElementChild.textContent = t("ui.noPendingNativeApprovals");
      return;
    }
    container.querySelector('.approvals-empty')?.remove();
    for (const card of container.querySelectorAll('[data-approval-card]')) {
      if (!ids.has(card.dataset.approvalCard)) card.remove();
    }
    pending.forEach((approval, index) => {
      const detail = approvalDetail(approval);
      let card = container.querySelector(`[data-approval-card="${CSS.escape(approval.id)}"]`);
      // Approval payloads are immutable while pending. Keep the actual controls
      // through unrelated events and snapshot reads, not just their text values.
      const key = JSON.stringify([approval, document.documentElement.lang]);
      if (!card || card.dataset.renderKey !== key) {
        const previous = card;
        const controls = Array.from(previous?.querySelectorAll('input') || []).map((input) => ({
          value: input.value, checked: input.checked, focused: input === document.activeElement,
          start: input.selectionStart, end: input.selectionEnd,
        }));
        const expanded = Array.from(previous?.querySelectorAll('details') || []).map((node) => node.open);
        card = document.createElement('section');
        card.className = `approval-card approval-${approval.agent}`;
        card.dataset.approvalCard = approval.id;
        card.dataset.renderKey = key;
        const title = document.createElement('div');
        title.className = 'approval-title';
        title.textContent = approval.title || t('room.nativeAgentRequest');
        const meta = document.createElement('div');
        meta.className = 'approval-meta';
        meta.textContent = `${displayName(approval.agent)} · ${approval.kind} · ${formatTime(approval.requested_at)}`;
        card.append(title, meta);
        if (approval.kind === 'claude.userQuestion') {
          renderClaudeQuestions(card, approval, detail);
        } else {
          const summary = document.createElement('div');
          summary.className = 'approval-summary';
          summary.textContent = approvalSummary(approval, detail);
          const raw = document.createElement('details');
          raw.className = 'approval-raw';
          const rawTitle = document.createElement('summary');
          rawTitle.textContent = t("ui.viewFullNativeRequest");
          const rawBody = document.createElement('pre');
          rawBody.className = 'approval-detail';
          rawBody.textContent = prettyJSON(approval.detail);
          raw.append(rawTitle, rawBody);
          const actions = document.createElement('div');
          actions.className = 'approval-actions';
          if (approval.kind === 'grok.permission') {
            // ACP labels and option IDs belong to the native runtime. Do not
            // collapse them into a broader permission or invent a session scope.
            for (const option of Array.isArray(detail.options) ? detail.options : []) {
              if (!option || typeof option.optionId !== 'string' || !option.optionId || typeof option.name !== 'string' || !option.name) continue;
              const allow = String(option.kind || '').startsWith('allow_');
              actions.appendChild(approvalButton(approval.id, `option:${option.optionId}`, option.name, allow ? 'approve-button' : 'decline-button'));
            }
            actions.appendChild(approvalButton(approval.id, 'cancel', t('ui.cancel'), 'decline-button'));
          } else {
            actions.appendChild(approvalButton(approval.id, 'accept', t("ui.allowOnce"), 'approve-button'));
            if (String(approval.kind).endsWith('/requestApproval') ||
                (approval.kind === 'claude.toolApproval' && Array.isArray(detail.permission_suggestions) && detail.permission_suggestions.length)) {
              actions.appendChild(approvalButton(approval.id, 'acceptForSession', t("ui.allowForThisSession"), 'approve-button secondary-approve'));
            }
            actions.appendChild(approvalButton(approval.id, 'decline', t("ui.reject"), 'decline-button'));
          }
          card.append(summary, raw, actions);
        }
        if (previous) previous.replaceWith(card);
        else container.appendChild(card);
        // A locale change may rebuild labels. Restore edits only when the
        // underlying native request has not changed.
        if (previous?.dataset.requestKey === JSON.stringify(approval)) {
          card.querySelectorAll('input').forEach((input, i) => {
            const saved = controls[i];
            if (!saved) return;
            input.value = saved.value; input.checked = saved.checked;
            if (saved.focused) {
              input.focus({ preventScroll: true });
              if (saved.start !== null) input.setSelectionRange(saved.start, saved.end);
            }
          });
          card.querySelectorAll('details').forEach((node, i) => { node.open = expanded[i] || false; });
        }
        card.dataset.requestKey = JSON.stringify(approval);
      }
      // Do not detach/reinsert unchanged cards: that also loses keyboard focus.
      if (container.children[index] !== card) container.insertBefore(card, container.children[index] || null);
      setApprovalBusy(card, state.approvalSubmissions.has(approval.id));
    });
  }

  function setApprovalBusy(card, busy) {
    if (!card) return;
    card.classList.toggle('submitting', busy);
    card.setAttribute('aria-busy', String(busy));
    card.querySelectorAll('button, input').forEach((control) => { control.disabled = busy; });
  }

  function approvalDetail(approval) {
    const value = approval?.detail;
    if (!value) return {};
    if (typeof value === 'object') return value;
    try { return JSON.parse(value); } catch { return { raw: String(value) }; }
  }

  function approvalSummary(approval, detail) {
    const input = detail.input && typeof detail.input === 'object' ? detail.input : detail;
    const call = detail.toolCall || {};
    const commandValue = input.command || input.cmd || call.rawInput?.command || '';
    const command = Array.isArray(commandValue) ? commandValue.join(' ') : commandValue;
    const path = input.file_path || input.path || input.cwd || detail.path || '';
    const description = detail.description || input.description || detail.reason || '';
    const tool = detail.tool_name || detail.method || call.title || approval.kind;
    return [tool, command ? t("ui.commandValue", { value0: command }) : '', path ? t("ui.pathValue", { value0: path }) : '', description].filter(Boolean).join('\n');
  }

  function renderClaudeQuestions(card, approval, detail) {
    const questions = Array.isArray(detail?.input?.questions) ? detail.input.questions : [];
    const questionTexts = questions.map((question) => question?.question);
    if (!questions.length || questionTexts.some((text) => typeof text !== 'string' || !text.trim()) || new Set(questionTexts).size !== questions.length) {
      const warning = document.createElement('div');
      warning.className = 'approval-summary approval-warning';
      warning.textContent = t("ui.claudeAskedAnInteractiveQuestionWithoutAParseableListItCanOnly");
      card.appendChild(warning);
      const actions = document.createElement('div');
      actions.className = 'approval-actions';
      actions.appendChild(approvalButton(approval.id, 'decline', t("ui.reject"), 'decline-button'));
      card.appendChild(actions);
      return;
    }

    const form = document.createElement('form');
    form.className = 'question-form';
    form.dataset.questionForm = approval.id;
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      submitQuestionApproval(approval.id, form.querySelector('[data-question-submit]'));
    });
    form.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && (event.isComposing || event.keyCode === 229 || event.repeat)) event.preventDefault();
    });
    questions.forEach((question, index) => {
      const text = String(question.question || question.header || t('room.questionNumber', { value: index + 1 }));
      const block = document.createElement('fieldset');
      block.className = 'question-block';
      block.dataset.questionText = text;
      const multiSelect = Boolean(question.multiSelect ?? question.multi_select);
      block.dataset.multiSelect = multiSelect ? 'true' : 'false';
      const legend = document.createElement('legend');
      legend.textContent = text;
      block.appendChild(legend);
      if (question.header && question.header !== text) {
        const header = document.createElement('div');
        header.className = 'question-header';
        header.textContent = question.header;
        block.appendChild(header);
      }

      const options = Array.isArray(question.options) ? question.options : [];
      const inputType = multiSelect ? 'checkbox' : 'radio';
      options.forEach((option, optionIndex) => {
        const label = document.createElement('label');
        label.className = 'question-option';
        const input = document.createElement('input');
        input.type = inputType;
        input.name = `question-${approval.id}-${index}`;
        input.value = String(option.label || option.value || t('room.optionNumber', { value: optionIndex + 1 }));
        const copy = document.createElement('span');
        const strong = document.createElement('strong');
        strong.textContent = input.value;
        copy.appendChild(strong);
        if (option.description) {
          const description = document.createElement('small');
          description.textContent = option.description;
          copy.appendChild(description);
        }
        label.append(input, copy);
        block.appendChild(label);
      });

      const other = document.createElement('input');
      other.type = 'text';
      other.className = 'question-other';
      other.placeholder = options.length ? t("ui.otherAnswerOptional") : t("ui.enterAnAnswer");
      other.dataset.questionOther = 'true';
      other.setAttribute('aria-label', text);
      block.appendChild(other);
      form.appendChild(block);
    });

    const actions = document.createElement('div');
    actions.className = 'approval-actions';
    const submit = document.createElement('button');
    submit.type = 'submit';
    submit.className = 'approve-button';
    submit.dataset.questionSubmit = approval.id;
    submit.textContent = t("ui.submitAnswers");
    actions.append(submit, approvalButton(approval.id, 'decline', t("ui.reject"), 'decline-button'));
    form.appendChild(actions);
    card.appendChild(form);
  }

  function collectQuestionAnswers(approvalId) {
    const form = document.querySelector(`[data-question-form="${CSS.escape(approvalId)}"]`);
    if (!form) return null;
    const answers = {};
    for (const block of form.querySelectorAll('.question-block')) {
      const question = block.dataset.questionText || '';
      const selected = Array.from(block.querySelectorAll('input[type="radio"]:checked, input[type="checkbox"]:checked')).map((input) => input.value);
      const other = block.querySelector('[data-question-other]')?.value.trim() || '';
      if (other) selected.push(other);
      if (!selected.length) {
        block.classList.add('invalid');
        block.querySelector('input')?.focus();
        return null;
      }
      block.classList.remove('invalid');
      answers[question] = selected.join(', ');
    }
    return answers;
  }

  function approvalButton(id, decision, label, className) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = className;
    button.dataset.approvalId = id;
    button.dataset.decision = decision;
    button.textContent = label;
    return button;
  }

  const MAX_IMAGE_BYTES = 5 * 1024 * 1024;
  const MAX_MESSAGE_IMAGE_BYTES = 20 * 1024 * 1024;
  const MAX_IMAGES_PER_MESSAGE = 8;
  const ACCEPTED_IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);

  async function addImageFiles(fileList) {
    const files = Array.from(fileList || []).filter((file) => file && file.size > 0);
    if (!files.length) return;
    let currentCount = state.pendingAttachments.length;
    let currentBytes = state.pendingAttachments.reduce((sum, item) => sum + Number(item.file?.size || item.attachment?.size || 0), 0);
    for (const file of files) {
      const duplicate = state.pendingAttachments.some((item) => item.file
        && item.file.name === file.name
        && item.file.size === file.size
        && item.file.lastModified === file.lastModified);
      if (duplicate) continue;
      if (currentCount >= MAX_IMAGES_PER_MESSAGE) {
        toast(t("ui.attachAtMostValueImagesToEachMessage", { value0: (MAX_IMAGES_PER_MESSAGE) }), 'error');
        break;
      }
      const extensionLooksValid = /\.(png|jpe?g|gif|webp)$/i.test(file.name || '');
      if (!ACCEPTED_IMAGE_TYPES.has(String(file.type || '').toLowerCase()) && !extensionLooksValid) {
        toast(t("ui.valueOnlyPngJpegGifAndWebpAreSupported", { value0: (file.name || t('ui.file')) }), 'error');
        continue;
      }
      if (file.size > MAX_IMAGE_BYTES) {
        toast(t("ui.valueExceedsThe5MibPerImageLimit", { value0: (file.name) }), 'error');
        continue;
      }
      if (currentBytes + file.size > MAX_MESSAGE_IMAGE_BYTES) {
        toast(t("ui.imagesOnThisMessageCannotExceed20MibTotal"), 'error');
        break;
      }
      const previewURL = URL.createObjectURL(file);
      state.attachmentObjectURLs.add(previewURL);
      const item = {
        key: window.crypto?.randomUUID ? window.crypto.randomUUID() : `upload-${Date.now()}-${Math.random()}`,
        file,
        previewURL,
        status: 'uploading',
        attachment: null,
        error: '',
        controller: null,
        removed: false,
      };
      state.pendingAttachments.push(item);
      currentCount += 1;
      currentBytes += file.size;
      void uploadPendingAttachment(item);
    }
    renderAttachmentStrip();
    updateComposerAvailability();
  }

  async function uploadPendingAttachment(item) {
    if (!item || item.removed) return;
    if (item.controller) item.controller.abort();
    item.controller = new AbortController();
    item.status = 'uploading';
    item.error = '';
    renderAttachmentStrip();
    updateComposerAvailability();
    const form = new FormData();
    form.append('file', item.file, item.file.name || 'image');
    try {
      const attachment = await api('/api/v1/attachments', { method: 'POST', body: form, signal: item.controller.signal });
      if (item.removed || !state.pendingAttachments.includes(item)) {
        await api(`/api/v1/attachments/${encodeURIComponent(attachment.id)}`, { method: 'DELETE' }).catch(() => {});
        return;
      }
      item.attachment = attachment;
      item.status = 'ready';
    } catch (error) {
      if (error.name === 'AbortError' || item.removed) return;
      item.status = 'error';
      item.error = error.message;
      toast(t("ui.valueUploadFailedValue", { value0: (item.file.name || t('ui.image')), value1: (error.message) }), 'error');
    } finally {
      item.controller = null;
      renderAttachmentStrip();
      updateComposerAvailability();
    }
  }

  function renderAttachmentStrip() {
    const strip = $('attachment-strip');
    strip.replaceChildren();
    strip.classList.toggle('hidden', state.pendingAttachments.length === 0);
    state.pendingAttachments.forEach((item) => {
      const card = document.createElement('div');
      card.className = `pending-attachment ${item.status}`;
      card.dataset.pendingAttachment = item.key;
      const image = document.createElement('img');
      image.src = item.previewURL;
      image.alt = item.file.name || t("ui.pendingImages");
      const meta = document.createElement('div');
      meta.className = 'pending-attachment-meta';
      const status = item.status === 'uploading' ? t("ui.uploading")
        : item.status === 'error' ? t("ui.failedValue", { value0: (truncate(item.error, 48)) })
          : `${item.attachment?.width && item.attachment?.height ? `${item.attachment.width}×${item.attachment.height} · ` : ''}${formatBytes(item.attachment?.size || item.file.size)}`;
      meta.textContent = `${item.file.name || 'image'} · ${status}`;
      meta.title = item.error || item.file.name || '';
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'remove-attachment';
      remove.dataset.removeAttachment = item.key;
      remove.disabled = Boolean(item.submitting);
      remove.setAttribute('aria-label', t("ui.removeValue", { value0: (item.file.name || t('ui.image')) }));
      remove.textContent = '×';
      card.append(image, meta, remove);
      if (item.status === 'error') {
        const retry = document.createElement('button');
        retry.type = 'button';
        retry.className = 'retry-upload';
        retry.dataset.retryUpload = item.key;
        retry.textContent = t("ui.retry");
        card.appendChild(retry);
      }
      strip.appendChild(card);
    });
  }

  async function removePendingAttachment(key) {
    const index = state.pendingAttachments.findIndex((item) => item.key === key);
    if (index < 0 || state.pendingAttachments[index].submitting) return;
    const [item] = state.pendingAttachments.splice(index, 1);
    item.removed = true;
    if (item.controller) item.controller.abort();
    if (item.previewURL) {
      URL.revokeObjectURL(item.previewURL);
      state.attachmentObjectURLs.delete(item.previewURL);
    }
    renderAttachmentStrip();
    updateComposerAvailability();
    if (item.attachment?.id) {
      api(`/api/v1/attachments/${encodeURIComponent(item.attachment.id)}`, { method: 'DELETE' }).catch(() => {
        // A durable transcript reference intentionally wins over composer cleanup.
      });
    }
  }

  function retryPendingAttachment(key) {
    const item = state.pendingAttachments.find((value) => value.key === key);
    if (!item || item.removed || item.status !== 'error') return;
    void uploadPendingAttachment(item);
  }

  function clearPendingAttachments(preserveServer = false, keys = null) {
    for (const item of state.pendingAttachments) {
      if (keys && !keys.has(item.key)) continue;
      if (!preserveServer && item.attachment?.id) {
        void api(`/api/v1/attachments/${encodeURIComponent(item.attachment.id)}`, { method: 'DELETE' }).catch(() => {});
      }
      if (item.controller) item.controller.abort();
      item.removed = true;
      if (item.previewURL) URL.revokeObjectURL(item.previewURL);
      state.attachmentObjectURLs.delete(item.previewURL);
    }
    state.pendingAttachments = state.pendingAttachments.filter((item) => !item.removed);
    renderAttachmentStrip();
    updateComposerAvailability();
  }

  function updateComposerAvailability() {
    const uploading = state.pendingAttachments.some((item) => item.status === 'uploading');
    const button = $('send-button');
    button.disabled = !state.snapshot || uploading || state.sending;
    button.setAttribute('aria-busy', String(state.sending));
    button.title = state.sending ? t('ui.sendingMessage') : uploading ? t('ui.sendAfterImageUploadsFinish') : '';
    button.dataset.i18nAriaLabel = state.sending ? 'ui.sendingMessage' : 'ui.sendMessage';
    button.setAttribute('aria-label', t(button.dataset.i18nAriaLabel));
    const label = button.querySelector('[data-i18n]');
    if (label) {
      label.dataset.i18n = state.sending ? 'ui.sendingMessage' : 'ui.send';
      label.textContent = t(label.dataset.i18n);
    }
  }

  async function sendMessage() {
    if (!state.snapshot || state.sending || state.composing) return;
    const draftText = messageInput.value;
    const draftRevision = state.draftRevision;
    const replyRevision = state.replyRevision;
    const replyTo = state.replyTo;
    const text = messageInput.value.trim();
    const uploading = state.pendingAttachments.some((item) => item.status === 'uploading');
    const failed = state.pendingAttachments.some((item) => item.status === 'error');
    const submitted = state.pendingAttachments.filter((item) => item.status === 'ready' && item.attachment);
    const attachments = submitted.map((item) => ({ id: item.attachment.id }));
    if (uploading) {
      toast(t("ui.waitForImageUploadsToFinish"), 'error');
      return;
    }
    if (failed) {
      toast(t("ui.retryOrRemoveImagesThatFailedToUpload"), 'error');
      return;
    }
    if (!text && attachments.length === 0) return;
    state.sending = true;
    submitted.forEach((item) => { item.submitting = true; });
    updateComposerAvailability();
    renderAttachmentStrip();
    try {
      await api('/api/v1/messages', {
        method: 'POST',
        body: JSON.stringify({
          text,
          to: recipientsForTarget(state.selectedTarget),
          reply_to: replyTo || undefined,
          attachments,
          intent: $('message-intent').value,
        }),
      });
      // Keep anything composed while the accepted request was in flight.
      if (state.draftRevision === draftRevision && messageInput.value === draftText) messageInput.value = '';
      persistComposerDraft();
      if (state.replyRevision === replyRevision) clearReply();
      clearPendingAttachments(true, new Set(submitted.map((item) => item.key)));
      autoSizeComposer();
      scrollBottom();
    } catch (error) {
      toast(error.message, 'error');
    } finally {
      state.sending = false;
      submitted.forEach((item) => { item.submitting = false; });
      renderAttachmentStrip();
      updateComposerAvailability();
      if (document.activeElement === $('send-button')) messageInput.focus();
    }
  }

  async function participantAction(actor, action) {
    if (participantBusy(actor)) return;
    state.participantActions.add(actor);
    renderParticipants();
    try {
      await api(`/api/v1/participants/${actor}/${action}`, { method: 'POST' });
      await refreshAfterWrite();
      toast(`${displayName(actor)}：${actionText(action)}`, 'success');
    } catch (error) {
      toast(error.message, 'error');
    } finally {
      state.participantActions.delete(actor);
      renderParticipants();
    }
  }

  async function saveSettings() {
    if (state.settingsSaving || !state.snapshot) return;
    if (!$('stall-disabled').checked && !$('stall-warning').reportValidity()) return;
    const revision = state.settingsRevision;
    const value = $('stall-disabled').checked ? -1 : Number($('stall-warning').value);
    state.settingsSaving = true;
    renderSettings();
    let accepted = false;
    try {
      await api('/api/v1/settings', { method: 'PUT', body: JSON.stringify({ stall_warning_seconds: value }) });
      accepted = true;
      // Keep local edits while waiting for authoritative post-write state.
      await refreshAfterWrite();
      if (state.settingsRevision === revision) state.settingsDirty = false;
      toast(t('ui.settingsSaved'), 'success');
    } catch (error) {
      toast(accepted ? t('room.settingsUnconfirmed', { detail: error.message }) : error.message, 'error');
    } finally {
      state.settingsSaving = false;
      renderSettings();
    }
  }

  async function retryMessage(messageId, target) {
    const key = messageActionKey(messageId, target);
    if (state.messageActions.has(key) || pendingRetry(messageId, target)) return;
    state.messageActions.add(key);
    renderMessage(messageId);
    try {
      await api(`/api/v1/messages/${encodeURIComponent(messageId)}/retry`, {
        method: 'POST', body: JSON.stringify({ to: [target] }),
      });
      await refreshAfterWrite();
      toast(t('ui.createdAnAuditableRetryMessageForValue', { value0: displayName(target) }), 'success');
    } catch (error) {
      toast(error.message, 'error');
    } finally {
      state.messageActions.delete(key);
      renderMessage(messageId);
    }
  }

  async function cancelMessage(messageId, target) {
    const key = messageActionKey(messageId, target);
    if (state.messageActions.has(key)) return;
    state.messageActions.add(key);
    renderMessage(messageId);
    try {
      await api(`/api/v1/messages/${encodeURIComponent(messageId)}/cancel`, {
        method: 'POST', body: JSON.stringify({ target }),
      });
      await refreshAfterWrite();
      toast(t('ui.requestedCancellationOfInFlightWorkForValue', { value0: displayName(target) }), 'success');
    } catch (error) {
      toast(t('ui.cancellationFailedValue', { value0: error.message }), 'error');
    } finally {
      state.messageActions.delete(key);
      renderMessage(messageId);
    }
  }

  async function downloadExport(format) {
    try {
      const response = await fetch(roomURL(`/api/v1/export?format=${encodeURIComponent(format)}`), { credentials: 'same-origin' });
      if (!response.ok) {
        const payload = await response.json().catch(() => ({}));
		throw new Error(window.PairRoomI18n?.errorMessage(payload) || payload.error || response.statusText);
      }
      const blob = await response.blob();
      const disposition = response.headers.get('content-disposition') || '';
      const match = disposition.match(/filename="([^"]+)"/i);
      const filename = match ? match[1] : `pairroom.${format === 'json' ? 'json' : 'md'}`;
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url;
      link.download = filename;
      document.body.appendChild(link);
      link.click();
      link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (error) {
      toast(error.message, 'error');
    }
  }

  async function setPermissions(actor, profile) {
    if (participantBusy(actor)) return;
    state.permissionSubmitting.add(actor);
    renderParticipants();
    try {
      await api(`/api/v1/participants/${actor}/permissions`, { method: 'PUT', body: JSON.stringify({ profile }) });
      toast(t('room.collaboration.permissionsUpdated'), 'success');
      // A read started before the write cannot confirm its result.
      await refreshAfterWrite();
    } catch (error) { toast(error.message, 'error'); }
    finally { state.permissionSubmitting.delete(actor); renderParticipants(); }
  }

  async function resolveApproval(id, decision, button, extra = {}) {
    if (state.approvalSubmissions.has(id)) return;
    const approval = (state.snapshot?.approvals || []).find((item) => item.id === id);
    if (!approval || approval.status !== 'pending') return;
    state.approvalSubmissions.set(id, 'submitting');
    setApprovalBusy(button.closest('[data-approval-card]'), true);
    try {
      await api(`/api/v1/approvals/${encodeURIComponent(id)}`, {
        method: 'POST',
        body: JSON.stringify({ decision, ...extra }),
      });
      // Remain disabled until durable SSE/snapshot state removes this request.
      if (state.approvalSubmissions.has(id)) state.approvalSubmissions.set(id, 'submitted');
      toast(decision === 'decline' || decision === 'cancel' ? t("ui.nativeRequestRejected") : t("ui.approvalSubmitted"), 'success');
    } catch (error) {
      state.approvalSubmissions.delete(id);
      const card = $('approvals-tab').querySelector(`[data-approval-card="${CSS.escape(id)}"]`);
      setApprovalBusy(card, false);
      toast(error.message, 'error');
    }
  }

  function submitQuestionApproval(id, button) {
    const answers = collectQuestionAnswers(id);
    if (!answers) {
      toast(t("ui.answerEveryQuestionBeforeSubmitting"), 'error');
      return;
    }
    void resolveApproval(id, 'accept', button, { answers });
  }

  async function refreshGitStatus() {
    const request = ++state.gitStatusRequest;
    $('git-status').removeAttribute('data-i18n');
    try {
      const result = await api('/api/v1/git/status');
      if (request === state.gitStatusRequest) $('git-status').textContent = result.text || t('room.workingTreeClean');
    } catch (error) {
      if (request === state.gitStatusRequest) $('git-status').textContent = error.message;
    }
  }

  async function refreshDiff() {
    const request = ++state.diffRequest;
    const staged = $('staged-diff').checked;
    const output = $('diff-output');
    output.removeAttribute('data-i18n');
    output.setAttribute('aria-busy', 'true');
    output.textContent = t('ui.readingDiff');
    try {
      const result = await api(`/api/v1/git/diff${staged ? '?staged=1' : ''}`);
      if (request === state.diffRequest && staged === $('staged-diff').checked) output.textContent = result.text || t('room.noChanges');
    } catch (error) {
      if (request === state.diffRequest) output.textContent = error.message;
    } finally {
      if (request === state.diffRequest) output.setAttribute('aria-busy', 'false');
    }
  }

  function setReply(messageId) {
    const message = state.snapshot.messages.find((item) => item.id === messageId);
    if (!message) return;
    state.replyRevision += 1;
    state.replyTo = messageId;
    if (['claude', 'codex'].includes(message.from)) setTarget(message.from);
    $('reply-preview').textContent = `${displayName(message.from)}：${truncate(message.text || attachmentSummary(message), 120)}`;
    $('reply-banner').classList.remove('hidden');
    messageInput.focus();
  }

  function clearReply() {
    state.replyRevision += 1;
    state.replyTo = '';
    $('reply-banner').classList.add('hidden');
    $('reply-preview').textContent = '';
  }

  function inspectMessage(messageId) {
    state.inspectorCorrelation = messageId;
    switchTab('activity');
    renderActivity();
  }

  function clearInspectorScope() {
    state.inspectorCorrelation = '';
    renderActivity();
  }

  function switchTab(tab) {
    state.inspectorTab = tab;
    document.querySelectorAll('.tab').forEach((button) => button.classList.toggle('active', button.dataset.tab === tab));
    document.querySelectorAll('.tab-content').forEach((panel) => panel.classList.toggle('active', panel.id === `${tab}-tab`));
    if (tab === 'diff') refreshDiff();
    if (tab === 'activity') renderActivity();
  }

  function recipientsForTarget(target) {
    return ['claude', 'codex'].includes(target) ? [target] : ['claude'];
  }

  function updateDeliveryHint() {
    $('delivery-hint').textContent = t('ui.sendOnlyToValue', { value0: displayName(state.selectedTarget) });
  }

  function setTarget(target) {
    if (!['claude', 'codex'].includes(target)) target = 'claude';
    state.selectedTarget = target;
    document.querySelectorAll('.target-button').forEach((button) => {
      const active = button.dataset.target === target;
      button.classList.toggle('active', active);
      button.setAttribute('aria-pressed', String(active));
    });
    updateDeliveryHint();
    persistComposerDraft();
    messageInput.focus();
  }

  function readLocal(key) {
    try { return localStorage.getItem(key); } catch { return null; }
  }

  function writeLocal(key, value) {
    try {
      if (value === null) localStorage.removeItem(key);
      else localStorage.setItem(key, value);
    } catch { /* Private/embedded contexts may deny browser storage. */ }
  }

  function initializeRoomLocalState() {
    const roomID = state.snapshot?.meta?.id || 'default';
    if (state.localRoomID === roomID) {
      recomputeUnread();
      return;
    }
    state.localRoomID = roomID;
    state.draftKey = `pairroom.draft.${roomID}`;
    const seenKey = `pairroom.lastSeen.${roomID}`;
    const storedSeen = Number(readLocal(seenKey) || 0);
    if (storedSeen > 0) state.lastSeenSeq = storedSeen;
    else {
      state.lastSeenSeq = Number(state.snapshot.latest_seq || 0);
      writeLocal(seenKey, String(state.lastSeenSeq));
    }
    try {
      const draft = JSON.parse(readLocal(state.draftKey) || 'null');
      if (draft && typeof draft === 'object' && state.draftRevision === 0) {
        messageInput.value = String(draft.text || '');
        if (['claude', 'codex'].includes(draft.target)) state.selectedTarget = draft.target;
        else if (['driver', 'reviewer'].includes(draft.target)) {
          const matching = ['claude', 'codex'].filter((actor) => state.snapshot?.participants[actor]?.role === draft.target);
          if (matching.length === 1) state.selectedTarget = matching[0];
        }
        if (['steer', 'queue'].includes(draft.intent)) $('message-intent').value = draft.intent;
      }
    } catch { writeLocal(state.draftKey, null); }
    setTarget(state.selectedTarget);
    autoSizeComposer();
    recomputeUnread();
    updateNotificationButton();
  }

  function persistComposerDraft() {
    if (!state.draftKey) return;
    const value = { text: messageInput.value, target: state.selectedTarget, intent: $('message-intent').value };
    writeLocal(state.draftKey, value.text ? JSON.stringify(value) : null);
  }

  function recomputeUnread() {
    state.unreadCount = (state.snapshot?.messages || []).filter((message) =>
      ['claude', 'codex'].includes(message.from) && Number(message.seq || 0) > state.lastSeenSeq).length;
    updateUnreadUI();
  }

  function handleIncomingMessage(message, seq) {
    if (!['claude', 'codex'].includes(message.from) || Number(seq || 0) <= state.lastSeenSeq) return;
    const nearBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 160;
    if (!document.hidden && nearBottom) {
      markConversationRead(true);
      return;
    }
    state.unreadCount += 1;
    updateUnreadUI();
    if ('Notification' in window && document.hidden && Notification.permission === 'granted') {
      const body = truncate(message.text || attachmentSummary(message), 180);
      const notification = new Notification(`${displayName(message.from)} · PairRoom`, { body, tag: `pairroom-${message.id}` });
      notification.onclick = () => { window.focus(); scrollToMessage(message.id); notification.close(); };
    }
  }

  function markConversationRead(force = false) {
    if (!state.snapshot || document.hidden || state.shellActive === false) return;
    const nearBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 160;
    if (!force && !nearBottom) return;
    const lastSeen = Number(state.snapshot.latest_seq || state.lastSeenSeq);
    // The shell re-asserts "active" after every surface report. Posting again when
    // nothing changed would ping-pong these messages forever and re-render the
    // Management tab strip (and rewrite localStorage) under the user's pointer.
    if (state.unreadCount === 0 && state.lastSeenSeq === lastSeen) return;
    state.lastSeenSeq = lastSeen;
    state.unreadCount = 0;
    writeLocal(`pairroom.lastSeen.${state.snapshot.meta.id}`, String(state.lastSeenSeq));
    updateUnreadUI();
  }

  function updateUnreadUI() {
    document.title = state.unreadCount > 0 ? `(${state.unreadCount}) PairRoom` : 'PairRoom';
    $('scroll-bottom').textContent = state.unreadCount > 0 ? t("ui.jumpToLatestValue", { value0: (state.unreadCount) }) : t("ui.jumpToLatest");
    postSurfaceState();
  }

  function pendingApprovalCount() {
    const approvals = state.snapshot?.approvals || [];
    return approvals.filter((item) => item.status === 'pending').length;
  }

  function postSurfaceState() {
    if (!EMBEDDED || window.parent === window) return;
    const roomId = state.snapshot?.meta?.id || '';
    window.parent.postMessage({
      type: 'pairroom-surface',
      roomId,
      name: state.snapshot?.meta?.name || '',
      unread: state.unreadCount || 0,
      pendingApprovals: pendingApprovalCount(),
      error: state.snapshot?.diagnostic || '',
      connected: Boolean(state.source && state.source.readyState === 1),
    }, window.location.origin);
  }

  async function requestNotifications() {
    if (!('Notification' in window)) { toast(t("ui.thisBrowserDoesNotSupportDesktopNotifications"), 'error'); return; }
    const permission = await Notification.requestPermission();
    updateNotificationButton();
    toast(permission === 'granted' ? t("ui.desktopNotificationsEnabled") : t("ui.desktopNotificationsAreOff"), permission === 'granted' ? 'success' : 'error');
  }

  function updateNotificationButton() {
    const button = $('notification-button');
    if (!('Notification' in window)) { button.disabled = true; button.textContent = '×'; return; }
    const granted = Notification.permission === 'granted';
    button.textContent = granted ? '◆' : '♢';
    button.title = granted ? t("ui.desktopNotificationsEnabled") : t("ui.enableDesktopNotifications");
    button.setAttribute('aria-pressed', String(granted));
    button.setAttribute('aria-label', granted ? t("ui.desktopNotificationsEnabledClickAgainToManageThem") : t("ui.enableDesktopNotifications"));
  }

  async function loadOlderMessages(button) {
    if (state.loadingOlder || !state.snapshot?.message_window?.has_more) return;
    const snapshot = state.snapshot;
    const oldest = (snapshot.messages || []).reduce((oldest, message) => {
      const seq = Number(message.seq || 0);
      return seq > 0 ? Math.min(oldest, seq) : oldest;
    }, Infinity);
    if (!Number.isFinite(oldest)) return;
    state.loadingOlder = true;
    if (button) button.disabled = true;
    const oldHeight = timeline.scrollHeight;
    const oldTop = timeline.scrollTop;
    try {
      const page = await api(`/api/v1/messages?before_seq=${oldest}&limit=100`);
      if (state.snapshot !== snapshot) return; // A resync supersedes this page.
      const existing = new Set((state.snapshot.messages || []).map((message) => message.id));
      const added = (page.messages || []).filter((message) => !existing.has(message.id));
      state.snapshot.messages = [...added, ...(state.snapshot.messages || [])].sort((a, b) => Number(a.seq) - Number(b.seq));
      state.snapshot.message_window = {
        total: Math.max(Number(page.total || 0), Number(state.snapshot.message_window?.total || 0), state.snapshot.messages.length),
        loaded: state.snapshot.messages.length,
        has_more: Boolean(page.has_more),
        oldest_seq: state.snapshot.messages[0]?.seq || 0,
      };
      renderTimeline();
      requestAnimationFrame(() => { timeline.scrollTop = oldTop + Math.max(0, timeline.scrollHeight - oldHeight); });
    } catch (error) {
      toast(t("ui.couldNotLoadMessageHistoryValue", { value0: (error.message) }), 'error');
    } finally {
      state.loadingOlder = false;
      queueRender();
    }
  }

  function setConnection(connected, labelKey) {
    const node = $('connection');
    node.classList.toggle('connected', connected);
    node.classList.toggle('disconnected', !connected);
    node.lastElementChild.dataset.i18n = labelKey;
    node.lastElementChild.textContent = t(labelKey);
    postSurfaceState();
  }

  function insertComposerText(text) {
    const start = Number.isInteger(messageInput.selectionStart) ? messageInput.selectionStart : messageInput.value.length;
    const end = Number.isInteger(messageInput.selectionEnd) ? messageInput.selectionEnd : start;
    messageInput.setRangeText(text, start, end, 'end');
    state.draftRevision += 1;
    persistComposerDraft();
    autoSizeComposer();
  }

  function autoSizeComposer() {
    messageInput.style.height = 'auto';
    messageInput.style.height = `${Math.min(messageInput.scrollHeight, 150)}px`;
  }

  function scrollBottom() { timeline.scrollTop = timeline.scrollHeight; }

  function toast(message, type = '') {
    const node = document.createElement('div');
    node.className = `toast ${type}`.trim();
    const text = document.createElement('span');
    text.className = 'toast-text';
    text.textContent = message;
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'toast-close';
    close.setAttribute('aria-label', t("ui.disableNotifications"));
    close.textContent = '×';
    $('toast-stack').appendChild(node);
    const duration = type === 'error' ? 8000 : 4500;
    let timer = window.setTimeout(() => node.remove(), duration);
    const dismiss = () => { window.clearTimeout(timer); node.remove(); };
    close.addEventListener('click', dismiss);
    // Pause auto-dismiss while the user reads the toast, then resume the window.
    node.addEventListener('mouseenter', () => window.clearTimeout(timer));
    node.addEventListener('mouseleave', () => { timer = window.setTimeout(() => node.remove(), duration); });
    node.append(text, close);
  }

  function participantPolicy(participant) {
    const runtime = participant.runtime || {};
    const readOnly = ['read-only', 'readOnly'].includes(runtime.sandbox) || runtime.permission_mode === 'plan' || participant.permission_profile === 'read-only' || (!participant.permission_profile && participant.role === 'reviewer');
    const policy = [runtime.permission_mode, runtime.approval_policy, runtime.sandbox].filter(Boolean).join(' · ') || t('common.inheritedPolicy');
    return { protected: readOnly, text: policy, title: t('room.collaboration.permissionBoundary') };
  }

  function participantLooksStalled(participant) {
    const seconds = Number(state.snapshot?.settings?.stall_warning_seconds ?? 300);
    if (seconds <= 0 || !['working', 'waiting'].includes(participant.state) || !participant.last_activity) return false;
    return Date.now() - new Date(participant.last_activity).getTime() >= seconds * 1000;
  }

  function displayName(actor) {
	if (actor === 'claude' || actor === 'codex') return state.snapshot?.participants?.[actor]?.display_name || (actor === 'claude' ? t('agent.agent1') : t('agent.agent2'));
    return ({ user: t('common.you'), system: 'PairRoom' })[actor] || actor;
  }
  function stateText(value) {
    return ({ stopped: t('common.stopped'), starting: t('common.starting'), idle: t('common.ready'), working: t('ui.working694b71b'), waiting: t('ui.waiting'), error: t('common.error') })[value] || value;
  }
  function responsibilityText(value) {
    return ({ lead: t('room.collaboration.lead'), executor: t('room.collaboration.executor') })[value] || t('room.collaboration.participant');
  }

  function deliveryText(value) {
    return ({ pending: t("ui.sending"), submitting: t("ui.submitting"), started: t("ui.startedANewTurn"), injected: t("ui.injectedIntoTheCurrentTurn"), queued: t("ui.queued"), failed: t("ui.failed"), skipped: t("ui.skipped") })[value] || value;
  }
  function processingText(value) {
    return ({ waiting: t("ui.waiting"), working: t("ui.working694b71b"), completed: t("ui.completed"), cancelled: t("ui.cancelled"), failed: t("ui.processingFailed") })[value] || value;
  }
  function isRetryable(message, target) {
    const processing = message.processing && message.processing[target];
    if (['failed', 'cancelled'].includes(processing)) return true;
    const delivery = message.delivery && message.delivery[target];
    return ['failed', 'skipped'].includes(delivery);
  }
  function actionText(value) {
    return ({ start: t("ui.started"), stop: t("ui.stopped"), restart: t("ui.restarted"), interrupt: t("ui.interruptRequested") })[value] || value;
  }
  function sessionSummary(p) {
    if (p.current_turn) return t('room.turnValue', { value: truncate(p.current_turn, 18) });
	const runtimeKind = p.runtime_kind || p.runtime?.runtime_kind || '';
    if (p.session_id) return runtimeKind === 'codex'
	  ? t('room.threadValue', { value: truncate(p.session_id, 18) })
	  : t('room.sessionValue', { value: truncate(p.session_id, 18) });
    return t("ui.waitingToStartTheNativeAgent");
  }
  function formatTime(value) {
	try { return window.PairRoomI18n.formatDate(value, { hour: '2-digit', minute: '2-digit', second: '2-digit' }); }
    catch { return ''; }
  }
  function truncate(value, length) {
    const text = String(value || '').replace(/\s+/g, ' ');
    return text.length > length ? `${text.slice(0, length)}…` : text;
  }
  function formatBytes(value) {
    const bytes = Number(value || 0);
    if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
    const units = ['B', 'KiB', 'MiB', 'GiB'];
    const index = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)));
    const amount = bytes / (1024 ** index);
    return `${window.PairRoomI18n.formatNumber(amount, { maximumFractionDigits: amount >= 10 || index === 0 ? 0 : 1 })} ${units[index]}`;
  }
  function attachmentSummary(message) {
    const attachments = window.PairRoomI18n.formatList((message?.attachments || []).map((item) => t("ui.imageValue", { value0: (item.name) })));
    return attachments || t("ui.imageMessage");
  }
  async function openAttachment(attachment) {
    try {
      const url = await loadAttachmentURL(attachment);
      openLightbox(attachment, url);
    } catch (error) {
      toast(t("ui.imageFailedToLoadValue", { value0: (error.message) }), 'error');
    }
  }
  function closeLightbox() {
    const dialog = $('image-lightbox');
    state.lightboxRequest += 1;
    state.lightboxItems = [];
    state.lightboxIndex = -1;
    state.lightboxRotation = 0;
    state.lightboxMode = 'fit';
    setLightboxZoom(1);
    updateLightboxMode();
    if (dialog.open) dialog.close();
    $('lightbox-image').removeAttribute('src');
    $('lightbox-open').href = '#';
    $('lightbox-open').removeAttribute('download');
    $('lightbox-counter').textContent = '';
  }

  function findAttachment(id) {
    for (const message of state.snapshot?.messages || []) {
      const found = (message.attachments || []).find((value) => value.id === id);
      if (found) return found;
    }
    const pending = state.pendingAttachments.find((value) => value.attachment?.id === id);
    return pending ? pending.attachment : null;
  }
  function scrollToMessage(id) {
    const node = timeline.querySelector(`[data-message-id="${CSS.escape(id)}"]`);
    if (!node) {
      toast(t("ui.theQuotedMessageIsHiddenByTheCurrentFilter"), 'error');
      return;
    }
    const reduceMotion = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    node.scrollIntoView({ behavior: reduceMotion ? 'auto' : 'smooth', block: 'center' });
    node.classList.remove('flash');
    requestAnimationFrame(() => node.classList.add('flash'));
    setTimeout(() => node.classList.remove('flash'), 1300);
  }
  async function copyText(value) {
    const text = String(value || '');
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return;
    }
    const temporary = document.createElement('textarea');
    temporary.value = text;
    temporary.setAttribute('readonly', '');
    temporary.style.position = 'fixed';
    temporary.style.opacity = '0';
    document.body.appendChild(temporary);
    temporary.select();
    const copied = document.execCommand('copy');
    temporary.remove();
    if (!copied) throw new Error(t('room.browserCopyCommandFailed'));
  }

  function prettyJSON(value) {
    if (value === null || value === undefined || value === '') return '';
    if (typeof value === 'string') {
      try { return JSON.stringify(JSON.parse(value), null, 2); } catch { return value; }
    }
    try { return JSON.stringify(value, null, 2); } catch { return String(value); }
  }
  function activityIcon(kind) {
    if (kind.includes('tool')) return '⌘';
    if (kind.includes('command')) return '›_';
    if (kind.includes('diff')) return '±';
    if (kind.includes('plan')) return '≡';
    if (kind.includes('approval')) return '!';
    if (kind.includes('error')) return '×';
    if (kind.includes('turn')) return '↻';
    return '·';
  }
  function activityLabel(event) {
    const labels = {
      'turn.started': t('room.turnStarted'), 'turn.completed': t('room.turnCompleted'),
      'runtime.info': t('room.runtimeCapabilities'),
      'input.processing': t('room.inputProcessing'), 'input.completed': t('room.inputCompleted'),
      'input.cancelled': t('room.inputCancelled'), 'input.failed': t('room.inputFailed'),
      'tool.started': t('room.toolState', { value: event.name || t('ui.started') }), 'tool.completed': t('room.toolState', { value: event.name || t('ui.completed') }),
      'command.output': t('room.commandOutput'), 'diff.updated': t('room.diffUpdated'), 'plan.updated': t('room.planUpdated'),
      'usage.updated': t('room.usageUpdated'), 'approval.requested': t('room.approvalRequested'),
      final: t('room.finalResponse'), log: event.name || t('room.runtimeLog'), error: t('room.runtimeError'),
    };
    return labels[event.kind] || event.kind;
  }
  function activityDetail(event) {
    if (event.text) return truncate(event.text, 1800);
    if (event.data) return truncate(prettyJSON(event.data), 1800);
    return '';
  }

  document.addEventListener('click', (event) => {
    const loadOlder = event.target.closest('[data-load-older]');
    if (loadOlder) { void loadOlderMessages(loadOlder); return; }
    const targetButton = event.target.closest('.target-button');
    if (targetButton) setTarget(targetButton.dataset.target);
    const action = event.target.closest('[data-action][data-actor]');
    if (action) participantAction(action.dataset.actor, action.dataset.action, action);
    const reply = event.target.closest('[data-reply-id]');
    if (reply) setReply(reply.dataset.replyId);
    const inspect = event.target.closest('[data-inspect-id]');
    if (inspect) inspectMessage(inspect.dataset.inspectId);
    const thread = event.target.closest('[data-thread-id]');
    if (thread) focusThread(thread.dataset.threadId);
    const tab = event.target.closest('.tab');
    if (tab) switchTab(tab.dataset.tab);
    const approval = event.target.closest('[data-approval-id][data-decision]');
    if (approval) void resolveApproval(approval.dataset.approvalId, approval.dataset.decision, approval);
    const retry = event.target.closest('[data-retry-id][data-retry-target]');
    if (retry) retryMessage(retry.dataset.retryId, retry.dataset.retryTarget, retry);
	const cancelMessageButton = event.target.closest('[data-cancel-message][data-cancel-target]');
	if (cancelMessageButton) cancelMessage(cancelMessageButton.dataset.cancelMessage, cancelMessageButton.dataset.cancelTarget, cancelMessageButton);
    const copy = event.target.closest('[data-copy-message]');
    if (copy) {
      const message = (state.snapshot.messages || []).find((item) => item.id === copy.dataset.copyMessage);
      if (message) copyText(message.text || attachmentSummary(message)).then(() => toast(t("ui.messageCopied"), 'success')).catch(() => toast(t("ui.clipboardIsUnavailable"), 'error'));
    }
    const expand = event.target.closest('[data-expand-message]');
    if (expand) {
      const id = expand.dataset.expandMessage;
      if (state.expandedMessages.has(id)) state.expandedMessages.delete(id);
      else state.expandedMessages.add(id);
      queueRender();
    }
    const scroll = event.target.closest('[data-scroll-message], [data-jump-message]');
    if (scroll) scrollToMessage(scroll.dataset.scrollMessage || scroll.dataset.jumpMessage);
    const openImage = event.target.closest('[data-open-attachment]');
    if (openImage) {
      const attachment = findAttachment(openImage.dataset.openAttachment);
      if (attachment) openAttachment(attachment);
    }
    const removeAttachment = event.target.closest('[data-remove-attachment]');
    if (removeAttachment) void removePendingAttachment(removeAttachment.dataset.removeAttachment);
    const retryUpload = event.target.closest('[data-retry-upload]');
    if (retryUpload) retryPendingAttachment(retryUpload.dataset.retryUpload);
  });
  document.addEventListener('change', (event) => {
    if (event.target.matches('[data-permission-actor]')) setPermissions(event.target.dataset.permissionActor, event.target.value);
  });
  $('send-button').addEventListener('click', () => {
    state.composing = false;
    sendMessage();
  });
  $('attach-button').addEventListener('click', () => $('attachment-input').click());
  $('attachment-input').addEventListener('change', (event) => { void addImageFiles(event.target.files); event.target.value = ''; });
  messageInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey && !event.altKey && !event.repeat
      && !event.isComposing && event.keyCode !== 229 && !state.composing) {
      event.preventDefault();
      sendMessage();
    }
  });
  messageInput.addEventListener('compositionstart', () => { state.composing = true; });
  messageInput.addEventListener('compositionend', () => { state.composing = false; });
  messageInput.addEventListener('compositioncancel', () => { state.composing = false; });
  messageInput.addEventListener('input', () => { state.draftRevision += 1; autoSizeComposer(); persistComposerDraft(); });
  messageInput.addEventListener('paste', (event) => {
    const files = Array.from(event.clipboardData?.files || []);
    if (!files.length) return;
    const text = event.clipboardData?.getData('text/plain') || '';
    event.preventDefault();
    if (text) insertComposerText(text);
    void addImageFiles(files);
  });
  $('cancel-reply').addEventListener('click', clearReply);
  $('clear-timeline-scope').addEventListener('click', clearThreadFilter);
  $('save-settings').addEventListener('click', saveSettings);
  $('stall-disabled').addEventListener('change', editSettings);
  $('stall-warning').addEventListener('input', editSettings);
  $('refresh-button').addEventListener('click', () => { void loadSnapshot().catch((error) => toast(error.message, 'error')); });
  $('message-search').addEventListener('input', (event) => {
    state.searchQuery = event.target.value;
    timeline.scrollTop = 0;
    queueRender();
  });
  $('conversation-filter').addEventListener('change', (event) => {
    state.conversationFilter = event.target.value;
    timeline.scrollTop = 0;
    queueRender();
  });
  $('export-markdown').addEventListener('click', () => downloadExport('markdown'));
  $('export-json').addEventListener('click', () => downloadExport('json'));
  $('notification-button').addEventListener('click', requestNotifications);
  $('message-intent').addEventListener('change', persistComposerDraft);
  $('scroll-bottom').addEventListener('click', () => { scrollBottom(); markConversationRead(true); });
  timeline.addEventListener('scroll', () => markConversationRead(false), { passive: true });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) { markConversationRead(false); renderActivity(); } });
  document.addEventListener('pairroom:layout', renderActivity);
  $('refresh-diff').addEventListener('click', refreshDiff);
  $('staged-diff').addEventListener('change', refreshDiff);
  $('inspector-agent').addEventListener('change', (event) => { state.inspectorAgent = event.target.value; renderActivity(); });
  $('clear-inspector-scope').addEventListener('click', clearInspectorScope);
  $('lightbox-close').addEventListener('click', closeLightbox);
  $('lightbox-prev').addEventListener('click', () => moveLightbox(-1));
  $('lightbox-next').addEventListener('click', () => moveLightbox(1));
  $('lightbox-zoom-out').addEventListener('click', () => setLightboxZoom(state.lightboxZoom - 0.25));
  $('lightbox-zoom-reset').addEventListener('click', () => setLightboxZoom(1));
  $('lightbox-zoom-in').addEventListener('click', () => setLightboxZoom(state.lightboxZoom + 0.25));
  $('lightbox-rotate-left').addEventListener('click', () => rotateLightbox(-90));
  $('lightbox-rotate-right').addEventListener('click', () => rotateLightbox(90));
  $('lightbox-fit').addEventListener('click', () => setLightboxMode('fit'));
  $('lightbox-actual').addEventListener('click', () => setLightboxMode('actual'));
  $('lightbox-copy').addEventListener('click', copyLightboxImage);
  $('lightbox-stage').addEventListener('wheel', (event) => { if (!event.ctrlKey && !event.metaKey) return; event.preventDefault(); setLightboxZoom(state.lightboxZoom + (event.deltaY < 0 ? 0.25 : -0.25)); }, { passive: false });
  $('lightbox-image').addEventListener('dblclick', () => setLightboxZoom(state.lightboxZoom === 1 ? 2 : 1));
  $('image-lightbox').addEventListener('click', (event) => {
    if (event.target === $('image-lightbox')) closeLightbox();
  });
  let dragDepth = 0;
  window.addEventListener('dragenter', (event) => {
    if (!Array.from(event.dataTransfer?.types || []).includes('Files')) return;
    dragDepth += 1;
    $('drop-overlay').classList.remove('hidden');
  });
  window.addEventListener('dragleave', () => {
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) $('drop-overlay').classList.add('hidden');
  });
  window.addEventListener('dragover', (event) => {
    if (Array.from(event.dataTransfer?.types || []).includes('Files')) event.preventDefault();
  });
  window.addEventListener('drop', (event) => {
    dragDepth = 0;
    $('drop-overlay').classList.add('hidden');
    const files = Array.from(event.dataTransfer?.files || []);
    if (!files.length) return;
    event.preventDefault();
    void addImageFiles(files);
  });
  document.addEventListener('keydown', (event) => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLocaleLowerCase() === 'k' &&
      !$('image-lightbox').open && !document.querySelector('dialog[open]')) {
      event.preventDefault();
      $('message-search').focus();
    }
    if ($('image-lightbox').open) {
      if (event.key === 'ArrowLeft') { event.preventDefault(); moveLightbox(-1); return; }
      if (event.key === 'ArrowRight') { event.preventDefault(); moveLightbox(1); return; }
      if (event.key === '+' || event.key === '=') { event.preventDefault(); setLightboxZoom(state.lightboxZoom + 0.25); return; }
      if (event.key === '-') { event.preventDefault(); setLightboxZoom(state.lightboxZoom - 0.25); return; }
      if (event.key === '0') { event.preventDefault(); setLightboxZoom(1); return; }
      if (event.key.toLocaleLowerCase() === 'r') { event.preventDefault(); rotateLightbox(event.shiftKey ? -90 : 90); return; }
      if (event.key.toLocaleLowerCase() === 'f') { event.preventDefault(); setLightboxMode('fit'); return; }
    }
    if (event.key === 'Escape') {
      if ($('image-lightbox').open) closeLightbox();
      else if (state.replyTo) clearReply();
      else if (state.threadFilter) clearThreadFilter();
      else if (state.inspectorCorrelation) clearInspectorScope();
      else if (state.searchQuery) {
        state.searchQuery = '';
        $('message-search').value = '';
        queueRender();
      }
    }
  });

  window.addEventListener('beforeunload', (event) => {
    if (state.settingsDirty) { event.preventDefault(); event.returnValue = ''; }
    closeEvents();
    clearTimeout(state.reconnectTimer);
    state.attachmentObjectURLs.forEach((url) => URL.revokeObjectURL(url));
    state.mediaObjectURLs.forEach((value) => { if (typeof value === 'string') URL.revokeObjectURL(value); });
  });

  function showTimelineLoading() {
    if (!timeline.isConnected) return;
    timeline.replaceChildren();
    const shell = document.createElement('div');
    shell.className = 'timeline-loading';
    shell.setAttribute('role', 'status');
    shell.setAttribute('aria-label', t("ui.loadingTheCollaborationTimeline"));
    for (let i = 0; i < 4; i += 1) {
      const row = document.createElement('div');
      row.className = 'skeleton-row';
      const avatar = document.createElement('div');
      avatar.className = 'skeleton-avatar';
      avatar.setAttribute('aria-hidden', 'true');
      const lines = document.createElement('div');
      lines.className = 'skeleton-lines';
      const lineA = document.createElement('div');
      lineA.className = 'skeleton-line';
      lineA.style.width = `${42 + (i % 3) * 20}%`;
      const lineB = document.createElement('div');
      lineB.className = 'skeleton-line skeleton-line-short';
      lines.append(lineA, lineB);
      row.append(avatar, lines);
      shell.appendChild(row);
    }
    timeline.appendChild(shell);
  }

  function showTimelineError(message) {
    if (!timeline.isConnected) return;
    timeline.replaceChildren();
    const shell = document.createElement('div');
    shell.className = 'timeline-error';
    shell.setAttribute('role', 'alert');
    const symbol = document.createElement('div');
    symbol.className = 'error-orbit';
    symbol.setAttribute('aria-hidden', 'true');
    symbol.textContent = '!';
    const heading = document.createElement('h2');
    heading.textContent = t("ui.unableToLoadRoom");
    const text = document.createElement('p');
    text.textContent = message || t("ui.theBrowserSessionOrLocalServiceConnectionFailedReopenTheFullUrl");
    const retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'secondary-button';
    retry.textContent = t("ui.reloadRoom");
    retry.addEventListener('click', () => { bootRoom(); });
    shell.append(symbol, heading, text, retry);
    timeline.appendChild(shell);
  }

  window.addEventListener('message', (event) => {
    if (!EMBEDDED || event.origin !== window.location.origin || event.source !== window.parent) return;
    const data = event.data;
    if (!data || data.type !== 'pairroom-shell') return;
    if (data.action === 'inactive') state.shellActive = false;
    if (data.action === 'active') {
      state.shellActive = true;
      markConversationRead(true);
      renderActivity();
    }
  });

  function bootRoom() {
    showTimelineLoading();
    setConnection(false, 'ui.connecting');
    initializeSession().then(loadSnapshot).catch((error) => {
      toast(error.message, 'error');
      showTimelineError(error.message);
      setConnection(false, 'room.offline');
    });
  }

  document.addEventListener('pairroom:lang', () => {
    if (window.PairRoomI18n) window.PairRoomI18n.apply(document);
    if (state.snapshot) render();
  });

  bootRoom();
})();
