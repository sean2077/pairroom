(() => {
  'use strict';

  const t = (zh, en) => (window.PairRoomI18n && window.PairRoomI18n.t) ? window.PairRoomI18n.t(zh, en) : zh;

  const bootstrapParameters = new URLSearchParams(window.location.hash.replace(/^#/, ''));
  let bootstrapToken = bootstrapParameters.get('token') || '';
  if (bootstrapToken) {
    // Fragments are not sent in HTTP requests or Referer headers. Remove the
    // one-time bootstrap secret from the address bar immediately and keep it
    // only in memory until it is exchanged for an HttpOnly browser session.
    history.replaceState(null, '', `${window.location.pathname}${window.location.search}`);
  }
  const savedTheme = localStorage.getItem('pairroom.theme');
  const initialTheme = savedTheme || (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
  document.documentElement.dataset.theme = initialTheme;
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
    selectedTarget: 'driver',
    replyTo: '',
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
    theme: initialTheme,
    shellActive: true,
    source: null,
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
      const message = payload.error || (response.status === 401
        ? t('浏览器会话无效；请从 PairRoom 启动输出中的完整地址重新打开。')
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
      const message = payload && payload.error ? payload.error : String(payload || response.statusText);
      throw new Error(message);
    }
    return payload;
  }

  async function apiBlob(path) {
    const response = await fetch(roomURL(path), { credentials: 'same-origin' });
    if (!response.ok) {
      const payload = await response.json().catch(() => ({}));
      throw new Error(payload.error || response.statusText);
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
      if (pending.has('workflow')) renderWorkflow();
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

  async function loadSnapshot() {
    state.snapshot = await api('/api/v1/snapshot?message_limit=250');
    state.drafts = { claude: '', codex: '' };
    state.draftCorrelation = { claude: '', codex: '' };
    initializeRoomLocalState();
    if (state.snapshot?.meta?.id) document.body.dataset.roomId = state.snapshot.meta.id;
    render(true);
    connectEvents();
    refreshGitStatus();
    postSurfaceState();
  }

  function connectEvents() {
    if (state.source) state.source.close();
    const since = state.snapshot ? state.snapshot.latest_seq || 0 : 0;
    const query = new URLSearchParams({ since: String(since) });
    const source = new EventSource(roomURL(`/api/v1/events?${query}`));
    state.source = source;
    setConnection(false, 'Connecting');
    source.addEventListener('open', () => setConnection(true, 'Live'));
    source.addEventListener('error', () => setConnection(false, 'Reconnecting'));
    source.addEventListener('pairroom', (raw) => {
      try {
        const event = JSON.parse(raw.data);
        applyEvent(event);
      } catch (error) {
        toast(`事件解析失败：${error.message}`, 'error');
      }
    });
  }

  function applyEvent(event) {
    if (!state.snapshot) return;
    const durable = Number(event.seq || 0) > 0;
    const latest = Number(state.snapshot.latest_seq || 0);
    if (durable && event.seq <= latest) return;
    if (durable && latest > 0 && event.seq > latest + 1) {
      toast(`事件序列出现缺口（${latest} → ${event.seq}），正在重新同步`, 'error');
      loadSnapshot().catch((error) => toast(error.message, 'error'));
      return;
    }
    if (durable) state.snapshot.latest_seq = event.seq;
    state.snapshot.events = state.snapshot.events || [];
    state.snapshot.events.push(event);
    if (state.snapshot.events.length > 600) state.snapshot.events.splice(0, state.snapshot.events.length - 600);
    const data = event.data || {};
    let renderScope = 'full';
    let renderMessageID = '';

    switch (event.kind) {
      case 'room.settings.updated':
        state.snapshot.settings = data;
        renderScope = 'settings';
        break;
      case 'participant.updated':
        state.snapshot.participants[data.id] = data;
        renderScope = 'participants';
        break;
      case 'workflow.updated':
        state.snapshot.workflow = data;
        renderScope = 'workflow';
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
          message.processing[data.target] = data.state;
          message.processing_detail[data.target] = data.detail || '';
          message.processing_turn[data.target] = data.turn_id || '';
          message.processing_last_updated_at[data.target] = data.updated_at || new Date().toISOString();
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
    else if (renderScope === 'workflow') queueRuntimeRender(['workflow', 'composer']);
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
    if (!current || current === 'pending') return true;
    if (current === 'failed' || current === 'skipped') return false;
    if (next === 'failed' || next === 'skipped') return true;
    return current === next;
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
    $('room-name').textContent = state.snapshot.meta.name;
    $('repo-path').textContent = state.snapshot.meta.repo;
    renderParticipants();
    renderWorkflow();
    updateDeliveryHint();
    renderTurnOwnerBar();
    renderSettings();
    renderAttachmentStrip();
    updateComposerAvailability();
    renderTimeline();
    renderActivity();
    renderApprovals();
    if (forceBottom || nearBottom) requestAnimationFrame(scrollBottom);
  }

  function renderTurnOwnerBar() {
    const bar = $('turn-owner-bar');
    if (!bar || !state.snapshot) return;
    const messages = state.snapshot.messages || [];
    const live = ['started', 'injected', 'queued'];
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
        if (delivery[target] === 'pending' && processing[target] === 'waiting' && target !== owner) {
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
    label.textContent = owner ? `当前轮次 · ${displayName(owner)}` : '空闲';
    bar.replaceChildren(label);
    if (queued > 0) {
      const badge = document.createElement('span');
      badge.className = 'turn-queue-count';
      badge.textContent = `队列 ${queued}`;
      bar.appendChild(badge);
    }
  }

  function renderWorkflow() {
    const bar = $('workflow-bar');
    const workflow = state.snapshot.workflow;
    if (!workflow || !Array.isArray(workflow.stages) || !workflow.stages.length) {
      bar.classList.add('hidden');
      bar.replaceChildren();
      return;
    }
    bar.classList.remove('hidden');
    bar.replaceChildren();

    const header = document.createElement('div');
    header.className = 'workflow-header';
    const heading = document.createElement('div');
    const title = document.createElement('strong');
    title.textContent = 'Natural workflow';
    const status = document.createElement('span');
    status.className = `workflow-status status-${workflow.status || 'unknown'}`;
    status.textContent = workflowStatusText(workflow.status);
    heading.append(title, status);
    const goal = document.createElement('div');
    goal.className = 'workflow-goal';
    goal.textContent = truncate(workflow.goal || '', 180);
    goal.title = workflow.goal || '';
    heading.appendChild(goal);
    header.appendChild(heading);

    if (workflow.status === 'awaiting_approval') {
      const approve = document.createElement('button');
      approve.type = 'button';
      approve.className = 'workflow-approve';
      approve.textContent = `批准计划 v${workflow.revision || 1}`;
      approve.addEventListener('click', () => {
        messageInput.value = t('批准执行当前计划');
        persistComposerDraft();
        autoSizeComposer();
        void sendMessage();
      });
      header.appendChild(approve);
    }
    bar.appendChild(header);

    const stages = document.createElement('div');
    stages.className = 'workflow-stages';
    workflow.stages.forEach((stage, index) => {
      const chip = document.createElement('div');
      const current = index === Number(workflow.current_stage || 0);
      chip.className = `workflow-stage stage-${stage.status || 'pending'}${current ? ' current' : ''}`;
      const number = document.createElement('span');
      number.className = 'workflow-stage-number';
      number.textContent = String(index + 1);
      const copy = document.createElement('span');
      const name = document.createElement('strong');
      name.textContent = `${displayName(stage.actor)} · ${workflowModeText(stage.mode)}`;
      const stateLabel = document.createElement('small');
      stateLabel.textContent = workflowStageStatusText(stage.status);
      copy.append(name, stateLabel);
      chip.append(number, copy);
      stages.appendChild(chip);
      if (index < workflow.stages.length - 1) {
        const arrow = document.createElement('span');
        arrow.className = 'workflow-arrow';
        arrow.textContent = '→';
        stages.appendChild(arrow);
      }
    });
    bar.appendChild(stages);

    if (workflow.status === 'waiting_human') {
      const hint = document.createElement('div');
      hint.className = 'workflow-hint';
      hint.textContent = t('当前阶段正在等待你的选择；直接回复房间即可继续同一阶段。');
      bar.appendChild(hint);
    } else if (workflow.status === 'awaiting_approval') {
      const hint = document.createElement('div');
      hint.className = 'workflow-hint approval';
      hint.textContent = `执行阶段尚未启动。批准只绑定当前计划修订 v${workflow.revision || 1}；计划变化后需要重新批准。`;
      bar.appendChild(hint);
    }
  }

  function workflowStatusText(value) {
    return ({
      running: 'Running', waiting_human: 'Needs your input', awaiting_approval: 'Approval gate',
      completed: 'Completed', cancelled: 'Cancelled', failed: 'Failed', superseded: 'Superseded',
    })[value] || value || 'Unknown';
  }

  function workflowModeText(value) {
    return ({ plan: 'Plan', review: 'Review', execute: 'Execute', audit: 'Audit', discuss: 'Discuss' })[value] || value || 'Stage';
  }

  function workflowStageStatusText(value) {
    return ({ pending: 'Pending', running: 'Running', waiting_human: 'Needs input', completed: 'Done', cancelled: 'Cancelled', failed: 'Failed' })[value] || value || '';
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
      avatar.textContent = actor === 'claude' ? 'C' : 'X';
      card.appendChild(avatar);

      const main = document.createElement('div');
      main.className = 'participant-main';
      const title = document.createElement('div');
      title.className = 'participant-title';
      const strong = document.createElement('strong');
      strong.textContent = p.display_name || displayName(actor);
      const roleBadge = document.createElement('span');
      roleBadge.className = 'role-badge';
      roleBadge.textContent = roleText(p.role);
      title.append(strong, roleBadge);
      main.appendChild(title);

      const subtitle = document.createElement('div');
      subtitle.className = 'participant-subtitle';
      subtitle.textContent = p.last_error || sessionSummary(p);
      main.appendChild(subtitle);

      const copy = document.createElement('button');
      copy.type = 'button';
      copy.className = 'ghost-button compact-button session-copy';
      const vendorID = String(p.session_id || '').trim();
      if (!vendorID) {
        copy.disabled = true;
        copy.textContent = t('尚未生成');
        copy.title = t('原生 Session/Thread ID 将在首次被接受的 Turn 后生成');
      } else {
        copy.textContent = actor === 'codex' ? t('复制 Thread ID') : t('复制 Session ID');
        copy.title = t('复制完整 ID，用于 resume 原生会话');
        copy.addEventListener('click', async () => {
          try {
            await navigator.clipboard.writeText(vendorID);
            toast('已复制完整 ID', 'success');
          } catch {
            toast('复制失败', 'error');
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
      model.textContent = [p.runtime?.provider, p.model || 'native default'].filter(Boolean).join(' · ');
      meta.append(status, model);
      main.appendChild(meta);

      const runtime = p.runtime || {};
      if (runtime.protocol || runtime.version || runtime.path || runtime.command) {
        const runtimeLine = document.createElement('div');
        runtimeLine.className = 'runtime-line';
        const pieces = [runtime.version ? `v${runtime.version}` : '', runtime.protocol, runtime.available === false ? 'unavailable' : ''].filter(Boolean);
        runtimeLine.textContent = pieces.join(' · ') || runtime.command || 'runtime detected';
        runtimeLine.title = [runtime.path, ...(runtime.capabilities || [])].filter(Boolean).join('\n');
        main.appendChild(runtimeLine);
      }
      if (stalled) {
        const warning = document.createElement('div');
        warning.className = 'runtime-line runtime-warning';
        warning.textContent = t('长时间无可观察事件；静默本身不等于停滞，请在 Inspector 判断后再打断或重试');
        main.appendChild(warning);
      }
      if (runtime.warnings && runtime.warnings.length) {
        const warning = document.createElement('div');
        warning.className = 'runtime-line runtime-warning';
        warning.textContent = truncate(runtime.warnings.join(' · '), 160);
        warning.title = runtime.warnings.join('\n');
        main.appendChild(warning);
      }

      const policy = participantPolicy(actor, p);
      const policyLine = document.createElement('div');
      policyLine.className = `native-policy ${policy.protected ? 'protected' : ''}`;
      policyLine.textContent = policy.text;
      policyLine.title = policy.title;
      main.appendChild(policyLine);

	  const workspace = p.workspace || {};
	  if (workspace.kind) {
		const workspaceLine = document.createElement('div');
		workspaceLine.className = `workspace-boundary ${workspace.read_only ? 'protected' : ''}`;
		const parts = [workspace.kind === 'reviewer-snapshot' ? t('独立审查快照') : t('实时工作区')];
		if (workspace.dirty) parts.push(t('含未提交改动'));
		if (workspace.untracked_count) parts.push(`${workspace.untracked_count} 个未跟踪文件`);
		workspaceLine.textContent = parts.join(' · ');
		workspaceLine.title = [
		  workspace.path,
		  workspace.source_head ? `HEAD ${workspace.source_head}` : '',
		  workspace.patch_sha256 ? `snapshot ${workspace.patch_sha256}` : '',
		  ...(workspace.warnings || []),
		].filter(Boolean).join('\n');
		main.appendChild(workspaceLine);
	  }

      const roleSelect = document.createElement('select');
      roleSelect.className = 'role-select';
      roleSelect.dataset.roleActor = actor;
      [['driver', t('Driver · 实现')], ['reviewer', t('Reviewer · 独立审查')], ['peer', t('Peer · 平级讨论')]].forEach(([value, label]) => {
        const option = document.createElement('option');
        option.value = value;
        option.textContent = label;
        option.selected = p.role === value;
        roleSelect.appendChild(option);
      });
      main.appendChild(roleSelect);

      const actions = document.createElement('div');
      actions.className = 'agent-actions';
      if (p.state === 'stopped' || p.state === 'error') {
        actions.appendChild(actionButton(actor, 'start', t('启动')));
      } else {
        actions.appendChild(actionButton(actor, 'interrupt', t('打断')));
        actions.appendChild(actionButton(actor, 'restart', t('重启')));
        actions.appendChild(actionButton(actor, 'stop', t('停止'), true));
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
    return button;
  }

  function renderSettings() {
    $('max-hops').value = state.snapshot.settings.max_agent_hops;
    $('max-hops-value').value = state.snapshot.settings.max_agent_hops;
    const stall = Number(state.snapshot.settings.stall_warning_seconds ?? 300);
    $('stall-disabled').checked = stall < 0;
    $('stall-warning').disabled = stall < 0;
    $('stall-warning').value = stall < 0 ? 300 : stall;
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
        ? t('正在加载更早消息…')
        : `加载更早消息 · 已显示 ${windowInfo.loaded || state.snapshot.messages.length} / ${windowInfo.total}`;
      timeline.appendChild(older);
    }

    if (!items.length && !state.drafts.claude && !state.drafts.codex && !windowInfo?.has_more) {
      const empty = document.createElement('div');
      empty.className = 'timeline-empty';
      const inner = document.createElement('div');
      inner.innerHTML = '<div class="empty-orbit"></div><h2>开始一次三方协作</h2><p>向 Claude Code 与 Codex 同时提出任务。它们保留各自原生 Harness，并在这个公共房间讨论；你可以随时插话或改变方向。</p>';
      empty.appendChild(inner);
      timeline.appendChild(empty);
      return;
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
    if ((query || state.conversationFilter !== 'all' || state.threadFilter) && visibleCount === 0) {
      const empty = document.createElement('div');
      empty.className = 'timeline-empty';
      empty.innerHTML = t('<div><h2>没有匹配消息</h2><p>修改搜索、消息筛选或退出线程视图即可恢复完整时间线。</p></div>');
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
    $('timeline-scope-text').textContent = `${messages.length} 条消息 · ${summary}`;
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
    if (key === localDateKey(today)) label = t('今天');
    else if (key === localDateKey(yesterday)) label = t('昨天');
    else {
      label = new Intl.DateTimeFormat('zh-CN', {
        year: date.getFullYear() === today.getFullYear() ? undefined : 'numeric',
        month: 'long', day: 'numeric', weekday: 'short',
      }).format(date);
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
    avatar.textContent = actor === 'user' ? 'Y' : actor === 'claude' ? 'C' : 'X';

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
    inspect.textContent = t('过程');
    inspect.dataset.inspectId = message.id;
    const copy = document.createElement('button');
    copy.className = 'reply-action';
    copy.textContent = t('复制');
    copy.dataset.copyMessage = message.id;
    const thread = document.createElement('button');
    thread.className = 'reply-action';
    thread.textContent = t('线程');
    thread.dataset.threadId = message.thread_id || '';
    thread.title = t('仅查看这条讨论线程');
    const reply = document.createElement('button');
    reply.className = 'reply-action';
    reply.textContent = t('回复');
    reply.dataset.replyId = message.id;
    actions.append(inspect, copy, thread, reply);
    meta.append(author, time);
    if (message.retry_of) {
      const retryMarker = document.createElement('span');
      retryMarker.className = 'retry-marker';
      retryMarker.textContent = t('重试');
      retryMarker.title = `Retry of ${message.retry_of}`;
      meta.appendChild(retryMarker);
    }
	if (message.intent && message.intent !== 'append') {
	  const intentMarker = document.createElement('span');
	  intentMarker.className = `intent-marker intent-${message.intent}`;
	  intentMarker.textContent = message.intent === 'supersede' ? t('替代旧指令') : t('下一 Turn');
	  if (message.supersedes) {
		const count = Object.values(message.supersedes).reduce((sum, ids) => sum + (ids || []).length, 0);
		intentMarker.title = count ? `取代 ${count} 个在途消息目标` : '';
	  }
	  meta.appendChild(intentMarker);
	}
    if (message.handoff) {
      const handoffMarker = document.createElement('span');
      handoffMarker.className = 'intent-marker';
      handoffMarker.textContent = t('紧凑交接');
      handoffMarker.title = truncate(message.handoff, 320);
      meta.appendChild(handoffMarker);
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
        const parentSummary = parent.text || (parent.attachments || []).map((item) => item.name).join('、') || t('图片消息');
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
      expand.textContent = isExpanded ? t('收起消息') : t('展开完整消息');
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
      chip.title = [deliveryDetail, processingDetail, turn ? `Turn: ${turn}` : ''].filter(Boolean).join('\n');
      delivery.appendChild(chip);
      if (isRetryable(message, target)) {
        const retryButton = document.createElement('button');
        retryButton.className = 'retry-button';
        retryButton.dataset.retryId = message.id;
        retryButton.dataset.retryTarget = target;
        retryButton.textContent = `重试 ${displayName(target)}`;
        delivery.appendChild(retryButton);
      }
      if (processing === 'waiting' || processing === 'working') {
        const cancelButton = document.createElement('button');
        cancelButton.className = 'cancel-message-button';
        cancelButton.dataset.cancelMessage = message.id;
        cancelButton.dataset.cancelTarget = target;
        cancelButton.textContent = `取消 ${displayName(target)}`;
        cancelButton.title = status === 'pending'
          ? t('该消息仍在 Room FIFO 中；只移除这一项，不会打断任何原生 Turn')
          : t('该输入已进入原生 Runtime；取消可能中断该参与者当前整个 Turn，但不会删除 Room FIFO 中尚未提交的后续消息');
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
    avatar.textContent = actor === 'claude' ? 'C' : 'X';
    const content = document.createElement('div');
    content.className = 'message-content';
    const meta = document.createElement('div');
    meta.className = 'message-meta';
    const author = document.createElement('span');
    author.className = 'message-author';
    author.textContent = displayName(actor);
    const typing = document.createElement('span');
    typing.textContent = t('正在输入');
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
    chip.textContent = notice.text || 'PairRoom event';
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
      onCopyError: () => toast('浏览器不允许写入剪贴板', 'error'),
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
      ? `外部图片未自动加载：${alt || value}`
      : `未找到可安全预览的本地图片：${alt || value}`;
    placeholder.appendChild(label);
    if (/^https?:\/\//i.test(value)) {
      const link = document.createElement('a');
      link.href = value;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.textContent = t('打开链接');
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
    stage.title = `预览 ${attachment.name}`;
    stage.dataset.openAttachment = attachment.id;
    const loading = document.createElement('span');
    loading.className = 'image-loading';
    loading.textContent = t('加载图片…');
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
      failed.textContent = `图片加载失败：${error.message}`;
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
      $('lightbox-title').textContent = `图片加载失败：${attachment.name}`;
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
      toast('当前浏览器不支持复制图片', 'error');
      return;
    }
    try {
      const blob = await apiBlob(`/api/v1/attachments/${encodeURIComponent(attachment.id)}`);
      const type = blob.type || attachment.media_type || 'image/png';
      await navigator.clipboard.write([new ClipboardItem({ [type]: blob })]);
      toast('图片已复制到剪贴板', 'success');
    } catch (error) {
      toast(`复制图片失败：${error.message}`, 'error');
    }
  }

  function imageMeta(value) {
    const dimensions = value.width && value.height ? `${value.width}×${value.height}` : '';
    return [dimensions, formatBytes(value.size)].filter(Boolean).join(' · ');
  }

  function renderActivity() {
    const container = $('activity-tab');
    container.replaceChildren();
    const scope = $('inspector-scope');
    const scopedMessage = state.inspectorCorrelation
      ? (state.snapshot.messages || []).find((message) => message.id === state.inspectorCorrelation)
      : null;
    const turnIDs = new Set(scopedMessage ? Object.values(scopedMessage.processing_turn || {}).filter(Boolean) : []);
    if (scopedMessage) {
      scope.classList.remove('hidden');
      $('inspector-scope-text').textContent = `仅显示 ${displayName(scopedMessage.from)} 消息 ${truncate(scopedMessage.id, 18)} 的工作过程`;
    } else {
      scope.classList.add('hidden');
      $('inspector-scope-text').textContent = '';
    }

    const summaries = (state.snapshot.turns || [])
      .filter((turn) => state.inspectorAgent === 'all' || turn.agent === state.inspectorAgent)
      .filter((turn) => !scopedMessage || (turn.message_ids || []).includes(scopedMessage.id) || turnIDs.has(turn.turn_id))
      .sort((a, b) => String(b.updated_at || b.started_at).localeCompare(String(a.updated_at || a.started_at)))
      .slice(0, 40);
    if (summaries.length) {
      const title = document.createElement('div');
      title.className = 'activity-section-title';
      title.textContent = 'Turn summaries';
      container.appendChild(title);
      summaries.forEach((summary) => container.appendChild(renderTurnSummary(summary)));
    }

    const events = (state.snapshot.events || [])
      .filter((event) => event.kind === 'runtime.event')
      .map((event) => ({ seq: event.seq, ...event.data }))
      .filter((event) => state.inspectorAgent === 'all' || event.agent === state.inspectorAgent)
      .filter((event) => !['text.delta', 'state', 'session'].includes(event.kind))
      .filter((event) => !scopedMessage || event.correlation_id === scopedMessage.id || (event.turn_id && turnIDs.has(event.turn_id)))
      .slice(-100)
      .reverse();
    if (events.length) {
      const title = document.createElement('div');
      title.className = 'activity-section-title';
      title.textContent = 'Recent native events';
      container.appendChild(title);
    }
    for (const event of events) {
      const card = document.createElement('div');
      card.className = 'activity-card';
      const head = document.createElement('div');
      head.className = 'activity-card-head';
      const kind = document.createElement('div');
      kind.className = 'activity-kind';
      const icon = document.createElement('span');
      icon.className = 'activity-icon';
      icon.textContent = activityIcon(event.kind);
      const label = document.createElement('span');
      label.textContent = activityLabel(event);
      kind.append(icon, label);
      const agent = document.createElement('span');
      agent.className = 'activity-agent';
      agent.textContent = displayName(event.agent);
      head.append(kind, agent);
      card.appendChild(head);
      const detail = activityDetail(event);
      if (detail) {
        const body = document.createElement('div');
        body.className = 'activity-body';
        body.textContent = detail;
        card.appendChild(body);
      }
      container.appendChild(card);
    }
    if (!summaries.length && !events.length) {
      const empty = document.createElement('div');
      empty.className = 'activity-empty';
      empty.textContent = scopedMessage
        ? t('该消息暂时没有持久化工作摘要。')
        : t('Agent 的 Turn、工具调用、命令、计划、Diff 和运行日志会显示在这里。');
      container.appendChild(empty);
    }
  }

  function renderTurnSummary(summary) {
    const card = document.createElement('details');
    card.className = `turn-card turn-${summary.agent} status-${summary.status || 'unknown'}`;
    card.open = summary.status === 'working' || summary.status === 'waiting' || Boolean(state.inspectorCorrelation);
    const head = document.createElement('summary');
    head.className = 'turn-card-head';
    const title = document.createElement('div');
    title.className = 'turn-card-title';
    const status = document.createElement('span');
    status.className = `turn-status status-${summary.status || 'unknown'}`;
    status.textContent = turnStatusText(summary.status);
    const name = document.createElement('strong');
    name.textContent = `${displayName(summary.agent)} · ${truncate(summary.turn_id || summary.id, 20)}`;
    title.append(status, name);
    const meta = document.createElement('span');
    meta.className = 'turn-card-meta';
    meta.textContent = [
      summary.duration_millis ? formatDuration(summary.duration_millis) : '',
      `${(summary.items || []).length} items`,
      formatTime(summary.updated_at || summary.started_at),
    ].filter(Boolean).join(' · ');
    head.append(title, meta);
    card.appendChild(head);

    const body = document.createElement('div');
    body.className = 'turn-card-body';
    if (summary.error) body.appendChild(turnSection('Error', summary.error, 'error'));
    if (summary.plan) body.appendChild(turnSection('Plan', summary.plan));
    if (summary.diff) body.appendChild(turnSection('Diff', summary.diff));
    if (summary.final_text) body.appendChild(turnSection('Final', summary.final_text));
    const items = summary.items || [];
    if (items.length) {
      const list = document.createElement('div');
      list.className = 'turn-item-list';
      items.slice(-40).forEach((item) => {
        const row = document.createElement('div');
        row.className = `turn-item item-${item.kind || 'event'} status-${item.status || 'unknown'}`;
        const tag = document.createElement('span');
        tag.className = 'turn-item-tag';
        tag.textContent = item.kind || 'event';
        const text = document.createElement('span');
        text.className = 'turn-item-text';
        text.textContent = [item.name, item.detail ? truncate(item.detail, 380) : ''].filter(Boolean).join(' · ') || item.id;
        const itemStatus = document.createElement('span');
        itemStatus.className = 'turn-item-status';
        itemStatus.textContent = item.status || '';
        row.append(tag, text, itemStatus);
        list.appendChild(row);
      });
      body.appendChild(list);
    }
    if (summary.usage) body.appendChild(turnSection('Usage', prettyJSON(summary.usage)));
    card.appendChild(body);
    return card;
  }

  function turnSection(label, value, tone = '') {
    const details = document.createElement('details');
    details.className = `turn-section ${tone}`;
    const title = document.createElement('summary');
    title.textContent = label;
    const content = document.createElement('pre');
    content.textContent = value;
    details.append(title, content);
    return details;
  }

  function turnStatusText(status) {
    return ({ working: 'Working', waiting: 'Waiting', completed: 'Done', cancelled: 'Cancelled', failed: 'Failed' })[status] || (status || 'Unknown');
  }

  function formatDuration(milliseconds) {
    const value = Math.max(0, Number(milliseconds) || 0);
    if (value < 1000) return `${Math.round(value)} ms`;
    if (value < 60_000) return `${(value / 1000).toFixed(value < 10_000 ? 1 : 0)} s`;
    const minutes = Math.floor(value / 60_000);
    const seconds = Math.round((value % 60_000) / 1000);
    return `${minutes}m ${seconds}s`;
  }

  function renderApprovals() {
    const container = $('approvals-tab');
    container.replaceChildren();
    const pending = (state.snapshot.approvals || []).filter((item) => item.status === 'pending');
    $('approval-count').textContent = String(pending.length);
    if (!pending.length) {
      const empty = document.createElement('div');
      empty.className = 'approvals-empty';
      empty.textContent = t('当前没有待处理审批。Claude 的工具权限/交互问题与 Codex 的命令、文件和权限请求都会显示在这里。');
      container.appendChild(empty);
      return;
    }
    pending.forEach((approval) => {
      const detail = approvalDetail(approval);
      const card = document.createElement('section');
      card.className = `approval-card approval-${approval.agent}`;
      card.dataset.approvalCard = approval.id;

      const title = document.createElement('div');
      title.className = 'approval-title';
      title.textContent = approval.title || 'Native agent request';
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
        card.appendChild(summary);

        const raw = document.createElement('details');
        raw.className = 'approval-raw';
        const rawTitle = document.createElement('summary');
        rawTitle.textContent = t('查看完整原生请求');
        const rawBody = document.createElement('pre');
        rawBody.className = 'approval-detail';
        rawBody.textContent = prettyJSON(approval.detail);
        raw.append(rawTitle, rawBody);
        card.appendChild(raw);

        const actions = document.createElement('div');
        actions.className = 'approval-actions';
        actions.appendChild(approvalButton(approval.id, 'accept', t('允许一次'), 'approve-button'));
        if (approval.agent === 'codex' || detail.permission_suggestions) {
          actions.appendChild(approvalButton(approval.id, 'acceptForSession', t('本会话允许'), 'approve-button secondary-approve'));
        }
        actions.appendChild(approvalButton(approval.id, 'decline', t('拒绝'), 'decline-button'));
        card.appendChild(actions);
      }
      container.appendChild(card);
    });
  }

  function approvalDetail(approval) {
    const value = approval?.detail;
    if (!value) return {};
    if (typeof value === 'object') return value;
    try { return JSON.parse(value); } catch { return { raw: String(value) }; }
  }

  function approvalSummary(approval, detail) {
    const input = detail.input && typeof detail.input === 'object' ? detail.input : {};
    const command = Array.isArray(input.command) ? input.command.join(' ') : (input.command || input.cmd || '');
    const path = input.file_path || input.path || input.cwd || detail.path || '';
    const description = detail.description || input.description || '';
    const tool = detail.tool_name || detail.method || approval.kind;
    return [tool, command ? `命令：${truncate(command, 260)}` : '', path ? `路径：${path}` : '', description].filter(Boolean).join('\n');
  }

  function renderClaudeQuestions(card, approval, detail) {
    const questions = Array.isArray(detail?.input?.questions) ? detail.input.questions : [];
    if (!questions.length) {
      const warning = document.createElement('div');
      warning.className = 'approval-summary approval-warning';
      warning.textContent = t('Claude 发出了交互问题，但请求中没有可解析的问题列表。为安全起见只能拒绝。');
      card.appendChild(warning);
      const actions = document.createElement('div');
      actions.className = 'approval-actions';
      actions.appendChild(approvalButton(approval.id, 'decline', t('拒绝'), 'decline-button'));
      card.appendChild(actions);
      return;
    }

    const form = document.createElement('form');
    form.className = 'question-form';
    form.dataset.questionForm = approval.id;
    questions.forEach((question, index) => {
      const text = String(question.question || question.header || `Question ${index + 1}`);
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
        input.value = String(option.label || option.value || `Option ${optionIndex + 1}`);
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
      other.placeholder = options.length ? t('其他回答（可选）') : t('请输入回答');
      other.dataset.questionOther = 'true';
      block.appendChild(other);
      form.appendChild(block);
    });

    const actions = document.createElement('div');
    actions.className = 'approval-actions';
    const submit = document.createElement('button');
    submit.type = 'button';
    submit.className = 'approve-button';
    submit.dataset.questionSubmit = approval.id;
    submit.textContent = t('提交回答');
    actions.append(submit, approvalButton(approval.id, 'decline', t('拒绝'), 'decline-button'));
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
        toast(`每条消息最多添加 ${MAX_IMAGES_PER_MESSAGE} 张图片`, 'error');
        break;
      }
      const extensionLooksValid = /\.(png|jpe?g|gif|webp)$/i.test(file.name || '');
      if (!ACCEPTED_IMAGE_TYPES.has(String(file.type || '').toLowerCase()) && !extensionLooksValid) {
        toast(`${file.name || t('文件')}：仅支持 PNG、JPEG、GIF 和 WebP`, 'error');
        continue;
      }
      if (file.size > MAX_IMAGE_BYTES) {
        toast(`${file.name}：超过 5 MiB 单图限制`, 'error');
        continue;
      }
      if (currentBytes + file.size > MAX_MESSAGE_IMAGE_BYTES) {
        toast('本条消息的图片总大小不能超过 20 MiB', 'error');
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
      toast(`${item.file.name || t('图片')} 上传失败：${error.message}`, 'error');
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
      image.alt = item.file.name || t('待发送图片');
      const meta = document.createElement('div');
      meta.className = 'pending-attachment-meta';
      const status = item.status === 'uploading' ? t('上传中…')
        : item.status === 'error' ? `失败 · ${truncate(item.error, 48)}`
          : `${item.attachment?.width && item.attachment?.height ? `${item.attachment.width}×${item.attachment.height} · ` : ''}${formatBytes(item.attachment?.size || item.file.size)}`;
      meta.textContent = `${item.file.name || 'image'} · ${status}`;
      meta.title = item.error || item.file.name || '';
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'remove-attachment';
      remove.dataset.removeAttachment = item.key;
      remove.setAttribute('aria-label', `移除 ${item.file.name || '图片'}`);
      remove.textContent = '×';
      card.append(image, meta, remove);
      if (item.status === 'error') {
        const retry = document.createElement('button');
        retry.type = 'button';
        retry.className = 'retry-upload';
        retry.dataset.retryUpload = item.key;
        retry.textContent = t('重试');
        card.appendChild(retry);
      }
      strip.appendChild(card);
    });
  }

  async function removePendingAttachment(key) {
    const index = state.pendingAttachments.findIndex((item) => item.key === key);
    if (index < 0) return;
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

  function clearPendingAttachments(preserveServer = false) {
    for (const item of state.pendingAttachments) {
      if (!preserveServer && item.attachment?.id) {
        void api(`/api/v1/attachments/${encodeURIComponent(item.attachment.id)}`, { method: 'DELETE' }).catch(() => {});
      }
      if (item.controller) item.controller.abort();
      item.removed = true;
      if (item.previewURL) URL.revokeObjectURL(item.previewURL);
      state.attachmentObjectURLs.delete(item.previewURL);
    }
    state.pendingAttachments = [];
    renderAttachmentStrip();
    updateComposerAvailability();
  }

  function updateComposerAvailability() {
    const uploading = state.pendingAttachments.some((item) => item.status === 'uploading');
    $('send-button').disabled = uploading;
    $('send-button').title = uploading ? t('图片上传完成后才能发送') : '';
  }

  async function sendMessage() {
    const text = messageInput.value.trim();
    const uploading = state.pendingAttachments.some((item) => item.status === 'uploading');
    const failed = state.pendingAttachments.some((item) => item.status === 'error');
    const attachments = state.pendingAttachments.filter((item) => item.status === 'ready' && item.attachment).map((item) => ({ id: item.attachment.id }));
    if (uploading) {
      toast('请等待图片上传完成', 'error');
      return;
    }
    if (failed) {
      toast('请重试或移除上传失败的图片', 'error');
      return;
    }
    if (!text && attachments.length === 0) return;
    $('send-button').disabled = true;
    try {
      await api('/api/v1/messages', {
        method: 'POST',
        body: JSON.stringify({
          text,
          to: recipientsForTarget(state.selectedTarget),
          target_role: ['driver', 'reviewer'].includes(state.selectedTarget) ? state.selectedTarget : undefined,
          reply_to: state.replyTo || undefined,
          attachments,
		  intent: $('message-intent').value,
        }),
      });
      messageInput.value = '';
      persistComposerDraft();
      clearReply();
      clearPendingAttachments(true);
      autoSizeComposer();
      scrollBottom();
    } catch (error) {
      toast(error.message, 'error');
    } finally {
      updateComposerAvailability();
      messageInput.focus();
    }
  }

  async function participantAction(actor, action, button) {
    button.disabled = true;
    const old = button.textContent;
    button.textContent = '…';
    try {
      await api(`/api/v1/participants/${actor}/${action}`, { method: 'POST' });
      toast(`${displayName(actor)}：${actionText(action)}`, 'success');
    } catch (error) {
      toast(error.message, 'error');
    } finally {
      button.disabled = false;
      button.textContent = old;
    }
  }

  async function saveSettings() {
    try {
      await api('/api/v1/settings', {
        method: 'PUT',
        body: JSON.stringify({
          routing_mode: 'turns',
          max_agent_hops: Number($('max-hops').value),
          stall_warning_seconds: $('stall-disabled').checked ? -1 : Number($('stall-warning').value),
        }),
      });
      toast('轮次限制已保存', 'success');
    } catch (error) {
      toast(error.message, 'error');
    }
  }

  async function retryMessage(messageId, target, button) {
    button.disabled = true;
    const old = button.textContent;
    button.textContent = t('重试中…');
    try {
      await api(`/api/v1/messages/${encodeURIComponent(messageId)}/retry`, {
        method: 'POST',
        body: JSON.stringify({ to: [target] }),
      });
      toast(`已为 ${displayName(target)} 创建可审计的重试消息`, 'success');
    } catch (error) {
      toast(error.message, 'error');
      button.disabled = false;
      button.textContent = old;
    }
  }

	async function cancelMessage(messageId, target, button) {
	  button.disabled = true;
	  try {
		await api(`/api/v1/messages/${encodeURIComponent(messageId)}/cancel`, {
		  method: 'POST', body: JSON.stringify({ target }),
		});
		toast(`已请求取消 ${displayName(target)} 的在途处理`, 'success');
	  } catch (error) {
		toast(`取消失败：${error.message}`, 'error');
	  } finally {
		button.disabled = false;
	  }
	}

  async function downloadExport(format) {
    try {
      const response = await fetch(roomURL(`/api/v1/export?format=${encodeURIComponent(format)}`), { credentials: 'same-origin' });
      if (!response.ok) {
        const payload = await response.json().catch(() => ({}));
        throw new Error(payload.error || response.statusText);
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

  async function setRole(actor, role) {
    try {
      await api(`/api/v1/participants/${actor}/role`, { method: 'PUT', body: JSON.stringify({ role }) });
      toast(`${displayName(actor)} 已切换为 ${roleText(role)}`, 'success');
    } catch (error) {
      toast(error.message, 'error');
      await loadSnapshot();
    }
  }

  async function resolveApproval(id, decision, button, extra = {}) {
    button.disabled = true;
    const card = button.closest('[data-approval-card]');
    if (card) card.classList.add('submitting');
    try {
      await api(`/api/v1/approvals/${encodeURIComponent(id)}`, {
        method: 'POST',
        body: JSON.stringify({ decision, ...extra }),
      });
      toast(decision === 'decline' || decision === 'cancel' ? t('已拒绝原生请求') : t('审批决定已提交'), 'success');
    } catch (error) {
      toast(error.message, 'error');
      button.disabled = false;
      if (card) card.classList.remove('submitting');
    }
  }

  function submitQuestionApproval(id, button) {
    const answers = collectQuestionAnswers(id);
    if (!answers) {
      toast('请回答所有问题后再提交', 'error');
      return;
    }
    void resolveApproval(id, 'accept', button, { answers });
  }

  async function refreshGitStatus() {
    try {
      const result = await api('/api/v1/git/status');
      $('git-status').textContent = result.text || 'Working tree clean';
    } catch (error) {
      $('git-status').textContent = error.message;
    }
  }

  async function refreshDiff() {
    $('diff-output').textContent = t('读取 Diff…');
    try {
      const staged = $('staged-diff').checked ? '?staged=1' : '';
      const result = await api(`/api/v1/git/diff${staged}`);
      $('diff-output').textContent = result.text || 'No changes.';
    } catch (error) {
      $('diff-output').textContent = error.message;
    }
  }

  function setReply(messageId) {
    const message = state.snapshot.messages.find((item) => item.id === messageId);
    if (!message) return;
    state.replyTo = messageId;
    if (['claude', 'codex'].includes(message.from)) setTarget(message.from);
    $('reply-preview').textContent = `${displayName(message.from)}：${truncate(message.text || attachmentSummary(message), 120)}`;
    $('reply-banner').classList.remove('hidden');
    messageInput.focus();
  }

  function clearReply() {
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
  }

  function currentDriver() {
    const participants = state.snapshot?.participants || {};
    const drivers = ['claude', 'codex'].filter((actor) => participants[actor]?.role === 'driver');
    return drivers.length === 1 ? drivers[0] : '';
  }

  function currentReviewer() {
    const participants = state.snapshot?.participants || {};
    const reviewers = ['claude', 'codex'].filter((actor) => participants[actor]?.role === 'reviewer');
    return reviewers.length === 1 ? reviewers[0] : '';
  }

  function recipientsForTarget(target) {
    // Role targets are resolved atomically by the server. A role switch from
    // another browser cannot turn @Driver or @Reviewer into an explicit send to
    // the participant that used to hold that role.
    if (target === 'driver' || target === 'reviewer') return [];
    if (target === 'claude') return ['claude'];
    if (target === 'codex') return ['codex'];
    return [];
  }

  function updateDeliveryHint() {
    const driver = currentDriver();
    const reviewer = currentReviewer();
    const labels = {
      driver: driver ? `发送给当前 Driver · ${displayName(driver)}` : 'Driver 角色不唯一；请选择明确 Agent',
      reviewer: reviewer ? `发送给当前 Reviewer · ${displayName(reviewer)}` : 'Reviewer 角色不唯一；请选择明确 Agent',
      claude: t('仅发送给 Claude'),
      codex: t('仅发送给 Codex'),
    };
    $('delivery-hint').textContent = labels[state.selectedTarget] || labels.driver;
  }

  function setTarget(target) {
    if (!['driver', 'reviewer', 'claude', 'codex'].includes(target)) target = 'driver';
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

  function initializeRoomLocalState() {
    const roomID = state.snapshot?.meta?.id || 'default';
    state.draftKey = `pairroom.draft.${roomID}`;
    const seenKey = `pairroom.lastSeen.${roomID}`;
    const storedSeen = Number(localStorage.getItem(seenKey) || 0);
    if (storedSeen > 0) state.lastSeenSeq = storedSeen;
    else {
      state.lastSeenSeq = Number(state.snapshot.latest_seq || 0);
      localStorage.setItem(seenKey, String(state.lastSeenSeq));
    }
    try {
      const draft = JSON.parse(localStorage.getItem(state.draftKey) || 'null');
      if (draft && typeof draft === 'object') {
        messageInput.value = String(draft.text || '');
        if (['driver', 'reviewer', 'claude', 'codex'].includes(draft.target)) state.selectedTarget = draft.target;
        if (['append', 'next_turn', 'supersede'].includes(draft.intent)) $('message-intent').value = draft.intent;
      }
    } catch { localStorage.removeItem(state.draftKey); }
    setTarget(state.selectedTarget);
    autoSizeComposer();
    recomputeUnread();
    updateNotificationButton();
  }

  function persistComposerDraft() {
    if (!state.draftKey) return;
    const value = { text: messageInput.value, target: state.selectedTarget, intent: $('message-intent').value };
    if (!value.text) localStorage.removeItem(state.draftKey);
    else localStorage.setItem(state.draftKey, JSON.stringify(value));
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
    if (document.hidden && Notification.permission === 'granted') {
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
    localStorage.setItem(`pairroom.lastSeen.${state.snapshot.meta.id}`, String(state.lastSeenSeq));
    updateUnreadUI();
  }

  function updateUnreadUI() {
    document.title = state.unreadCount > 0 ? `(${state.unreadCount}) PairRoom` : 'PairRoom';
    $('scroll-bottom').textContent = state.unreadCount > 0 ? `跳到最新 (${state.unreadCount})` : '跳到最新';
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
    if (!('Notification' in window)) { toast('当前浏览器不支持桌面通知', 'error'); return; }
    const permission = await Notification.requestPermission();
    updateNotificationButton();
    toast(permission === 'granted' ? t('桌面通知已启用') : t('桌面通知未启用'), permission === 'granted' ? 'success' : 'error');
  }

  function updateNotificationButton() {
    const button = $('notification-button');
    if (!('Notification' in window)) { button.disabled = true; button.textContent = '×'; return; }
    const granted = Notification.permission === 'granted';
    button.textContent = granted ? '◆' : '♢';
    button.title = granted ? t('桌面通知已启用') : t('启用桌面通知');
    button.setAttribute('aria-pressed', String(granted));
    button.setAttribute('aria-label', granted ? t('桌面通知已启用，再次点击管理') : t('启用桌面通知'));
  }

  async function loadOlderMessages(button) {
    if (state.loadingOlder || !state.snapshot?.message_window?.has_more) return;
    const oldest = Math.min(...(state.snapshot.messages || []).map((message) => Number(message.seq || 0)).filter((value) => value > 0));
    if (!Number.isFinite(oldest)) return;
    state.loadingOlder = true;
    if (button) button.disabled = true;
    const oldHeight = timeline.scrollHeight;
    const oldTop = timeline.scrollTop;
    try {
      const page = await api(`/api/v1/messages?before_seq=${oldest}&limit=100`);
      const existing = new Set((state.snapshot.messages || []).map((message) => message.id));
      const added = (page.messages || []).filter((message) => !existing.has(message.id));
      state.snapshot.messages = [...added, ...(state.snapshot.messages || [])].sort((a, b) => Number(a.seq) - Number(b.seq));
      state.snapshot.message_window = {
        total: page.total,
        loaded: state.snapshot.messages.length,
        has_more: Boolean(page.has_more),
        oldest_seq: state.snapshot.messages[0]?.seq || 0,
      };
      renderTimeline();
      requestAnimationFrame(() => { timeline.scrollTop = oldTop + Math.max(0, timeline.scrollHeight - oldHeight); });
    } catch (error) {
      toast(`加载历史消息失败：${error.message}`, 'error');
    } finally {
      state.loadingOlder = false;
      queueRender();
    }
  }

  function setConnection(connected, label) {
    const node = $('connection');
    node.classList.toggle('connected', connected);
    node.classList.toggle('disconnected', !connected);
    node.lastElementChild.textContent = label;
    postSurfaceState();
  }

  function insertComposerText(text) {
    const start = Number.isInteger(messageInput.selectionStart) ? messageInput.selectionStart : messageInput.value.length;
    const end = Number.isInteger(messageInput.selectionEnd) ? messageInput.selectionEnd : start;
    messageInput.setRangeText(text, start, end, 'end');
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
    close.setAttribute('aria-label', t('关闭通知'));
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

  function participantPolicy(actor, participant) {
    const role = participant.role || 'peer';
    const runtime = participant.runtime || {};
    if (role === 'reviewer') {
      if (actor === 'claude') {
        return {
          protected: true,
          text: t('原生保护 · Plan mode'),
          title: t('Reviewer 使用 Claude Code 原生 plan permission mode；避免执行修改，但不是操作系统级文件隔离。'),
        };
      }
      return {
        protected: true,
        text: t('原生保护 · Read-only sandbox'),
        title: t('Reviewer 的每个 Codex turn 使用 App Server 原生 readOnly sandbox policy。'),
      };
    }
    if (actor === 'claude') {
      const mode = runtime.permission_mode || 'configured permission mode';
      return {
        protected: false,
        text: `${role === 'driver' ? t('写入者') : t('平级协作')} · ${mode}`,
        title: t('该角色按 Claude Code 当前 permission mode 工作，可能修改工作区。'),
      };
    }
    const sandbox = runtime.sandbox || 'workspaceWrite';
    return {
      protected: false,
      text: `${role === 'driver' ? t('写入者') : t('平级协作')} · ${sandbox}`,
      title: t('该角色按 Codex 当前 sandbox policy 工作，可能修改工作区。'),
    };
  }

  function participantLooksStalled(participant) {
    const seconds = Number(state.snapshot?.settings?.stall_warning_seconds ?? 300);
    if (seconds <= 0 || !['working', 'waiting'].includes(participant.state) || !participant.last_activity) return false;
    return Date.now() - new Date(participant.last_activity).getTime() >= seconds * 1000;
  }

  function displayName(actor) {
    return ({ user: 'You', claude: 'Claude Code', codex: 'Codex', system: 'PairRoom' })[actor] || actor;
  }
  function stateText(value) {
    return ({ stopped: 'Stopped', starting: 'Starting', idle: 'Ready', working: 'Working', waiting: 'Waiting', error: 'Error' })[value] || value;
  }
  function roleText(value) {
    return ({ driver: 'Driver', reviewer: 'Reviewer', peer: 'Peer' })[value] || value;
  }
  function deliveryText(value) {
    return ({ pending: t('发送中'), started: t('已开始新 Turn'), injected: t('已注入当前 Turn'), queued: t('已排队'), failed: t('失败'), skipped: t('已跳过') })[value] || value;
  }
  function processingText(value) {
    return ({ waiting: t('等待处理'), working: t('处理中'), completed: t('已完成'), cancelled: t('已取消'), failed: t('处理失败'), superseded: t('已被新指令取代') })[value] || value;
  }
  function isRetryable(message, target) {
    const processing = message.processing && message.processing[target];
    if (['failed', 'cancelled', 'superseded'].includes(processing)) return true;
    const delivery = message.delivery && message.delivery[target];
    return ['failed', 'skipped'].includes(delivery);
  }
  function actionText(value) {
    return ({ start: t('已启动'), stop: t('已停止'), restart: t('已重启'), interrupt: t('已请求打断') })[value] || value;
  }
  function sessionSummary(p) {
    if (p.current_turn) return `Turn ${truncate(p.current_turn, 18)}`;
    if (p.session_id) return `Session ${truncate(p.session_id, 18)}`;
    return t('等待启动原生 Agent');
  }
  function formatTime(value) {
    try { return new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(value)); }
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
    return `${amount >= 10 || index === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
  }
  function attachmentSummary(message) {
    const attachments = (message?.attachments || []).map((item) => `[图片] ${item.name}`).join('、');
    return attachments || t('图片消息');
  }
  async function openAttachment(attachment) {
    try {
      const url = await loadAttachmentURL(attachment);
      openLightbox(attachment, url);
    } catch (error) {
      toast(`图片加载失败：${error.message}`, 'error');
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
      toast('引用的消息当前被筛选隐藏', 'error');
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
    if (!copied) throw new Error('browser copy command failed');
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
      'turn.started': 'Turn started', 'turn.completed': 'Turn completed',
      'runtime.info': 'Runtime capabilities',
      'input.processing': 'Input processing', 'input.completed': 'Input completed',
      'input.cancelled': 'Input cancelled', 'input.failed': 'Input failed',
      'tool.started': `Tool · ${event.name || 'started'}`, 'tool.completed': `Tool · ${event.name || 'completed'}`,
      'command.output': 'Command output', 'diff.updated': 'Diff updated', 'plan.updated': 'Plan updated',
      'usage.updated': 'Usage updated', 'approval.requested': 'Approval requested',
      final: 'Final response', log: event.name || 'Runtime log', error: 'Runtime error',
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
    const questionSubmit = event.target.closest('[data-question-submit]');
    if (questionSubmit) submitQuestionApproval(questionSubmit.dataset.questionSubmit, questionSubmit);
    const approval = event.target.closest('[data-approval-id][data-decision]');
    if (approval) void resolveApproval(approval.dataset.approvalId, approval.dataset.decision, approval);
    const retry = event.target.closest('[data-retry-id][data-retry-target]');
    if (retry) retryMessage(retry.dataset.retryId, retry.dataset.retryTarget, retry);
	const cancelMessageButton = event.target.closest('[data-cancel-message][data-cancel-target]');
	if (cancelMessageButton) cancelMessage(cancelMessageButton.dataset.cancelMessage, cancelMessageButton.dataset.cancelTarget, cancelMessageButton);
    const copy = event.target.closest('[data-copy-message]');
    if (copy) {
      const message = (state.snapshot.messages || []).find((item) => item.id === copy.dataset.copyMessage);
      if (message) copyText(message.text || attachmentSummary(message)).then(() => toast('消息已复制', 'success')).catch(() => toast('无法访问剪贴板', 'error'));
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
    if (event.target.matches('[data-role-actor]')) setRole(event.target.dataset.roleActor, event.target.value);
  });
  $('send-button').addEventListener('click', sendMessage);
  $('attach-button').addEventListener('click', () => $('attachment-input').click());
  $('attachment-input').addEventListener('change', (event) => { void addImageFiles(event.target.files); event.target.value = ''; });
  messageInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      sendMessage();
    }
  });
  messageInput.addEventListener('input', () => { autoSizeComposer(); persistComposerDraft(); });
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
  $('max-hops').addEventListener('input', () => { $('max-hops-value').value = $('max-hops').value; });
  $('stall-disabled').addEventListener('change', () => { $('stall-warning').disabled = $('stall-disabled').checked; });
  $('refresh-button').addEventListener('click', loadSnapshot);
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
  $('theme-button').addEventListener('click', () => {
    state.theme = state.theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = state.theme;
    localStorage.setItem('pairroom.theme', state.theme);
    $('theme-button').setAttribute('aria-pressed', String(state.theme === 'light'));
  });
  $('theme-button').setAttribute('aria-pressed', String(initialTheme === 'light'));
  window.addEventListener('storage', (event) => {
    if (event.key !== 'pairroom.theme') return;
    const next = event.newValue === 'light' || event.newValue === 'dark'
      ? event.newValue
      : (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
    state.theme = next;
    document.documentElement.dataset.theme = next;
    $('theme-button').setAttribute('aria-pressed', String(next === 'light'));
  });
  $('scroll-bottom').addEventListener('click', () => { scrollBottom(); markConversationRead(true); });
  timeline.addEventListener('scroll', () => markConversationRead(false), { passive: true });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) markConversationRead(false); });
  $('refresh-diff').addEventListener('click', refreshDiff);
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

  window.addEventListener('beforeunload', () => {
    state.attachmentObjectURLs.forEach((url) => URL.revokeObjectURL(url));
    state.mediaObjectURLs.forEach((value) => { if (typeof value === 'string') URL.revokeObjectURL(value); });
  });

  function showTimelineLoading() {
    if (!timeline.isConnected) return;
    timeline.replaceChildren();
    const shell = document.createElement('div');
    shell.className = 'timeline-loading';
    shell.setAttribute('role', 'status');
    shell.setAttribute('aria-label', t('正在加载协作时间线'));
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
    heading.textContent = t('无法加载房间');
    const text = document.createElement('p');
    text.textContent = message || t('浏览器会话或本地 Service 连接失败。从 PairRoom 启动输出中的完整地址重新打开，或重试。');
    const retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'secondary-button';
    retry.textContent = t('重新加载房间');
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
    }
  });

  function bootRoom() {
    showTimelineLoading();
    setConnection(false, 'Connecting');
    initializeSession().then(loadSnapshot).catch((error) => {
      toast(error.message, 'error');
      showTimelineError(error.message);
      setConnection(false, 'Offline');
    });
  }

  document.addEventListener('pairroom:lang', () => {
    if (window.PairRoomI18n) window.PairRoomI18n.apply(document);
    if (state.snapshot) render(true);
  });

  bootRoom();
})();
