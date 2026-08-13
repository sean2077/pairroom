(() => {
  'use strict';

  const tokenQuery = new URLSearchParams(window.location.search).get('token') || '';
  if (tokenQuery) {
    sessionStorage.setItem('pairroom.token', tokenQuery);
    const cleanURL = new URL(window.location.href);
    cleanURL.searchParams.delete('token');
    history.replaceState(null, '', `${cleanURL.pathname}${cleanURL.search}${cleanURL.hash}`);
  }
  const token = tokenQuery || sessionStorage.getItem('pairroom.token') || '';
  const savedTheme = localStorage.getItem('pairroom.theme');
  const initialTheme = savedTheme || (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
  document.documentElement.dataset.theme = initialTheme;
  const state = {
    snapshot: null,
    drafts: { claude: '', codex: '' },
    selectedTarget: 'all',
    replyTo: '',
    inspectorAgent: 'all',
    inspectorTab: 'activity',
    inspectorCorrelation: '',
    searchQuery: '',
    theme: initialTheme,
    source: null,
    renderQueued: false,
  };

  const $ = (id) => document.getElementById(id);
  const timeline = $('timeline');
  const messageInput = $('message-input');

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (token) headers.set('Authorization', `Bearer ${token}`);
    if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await fetch(path, { ...options, headers });
    const type = response.headers.get('content-type') || '';
    const payload = type.includes('application/json') ? await response.json() : await response.text();
    if (!response.ok) {
      const message = payload && payload.error ? payload.error : String(payload || response.statusText);
      throw new Error(message);
    }
    return payload;
  }

  function queueRender() {
    if (state.renderQueued) return;
    state.renderQueued = true;
    requestAnimationFrame(() => {
      state.renderQueued = false;
      render();
    });
  }

  async function loadSnapshot() {
    state.snapshot = await api('/api/v1/snapshot');
    state.drafts = { claude: '', codex: '' };
    render(true);
    connectEvents();
    refreshGitStatus();
  }

  function connectEvents() {
    if (state.source) state.source.close();
    const since = state.snapshot ? state.snapshot.latest_seq || 0 : 0;
    const query = new URLSearchParams({ since: String(since) });
    if (token) query.set('token', token);
    const source = new EventSource(`/api/v1/events?${query}`);
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

    switch (event.kind) {
      case 'room.settings.updated':
        state.snapshot.settings = data;
        break;
      case 'participant.updated':
        state.snapshot.participants[data.id] = data;
        break;
      case 'message.created': {
        if (!state.snapshot.messages.some((item) => item.id === data.id)) {
          data.seq = event.seq;
          state.snapshot.messages.push(data);
        }
        if (data.from === 'claude' || data.from === 'codex') state.drafts[data.from] = '';
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
        break;
      }
      case 'approval.updated': {
        state.snapshot.approvals = state.snapshot.approvals || [];
        const index = state.snapshot.approvals.findIndex((item) => item.id === data.id);
        if (index >= 0) state.snapshot.approvals[index] = data;
        else state.snapshot.approvals.push(data);
        break;
      }
      case 'runtime.event':
        applyRuntime(data);
        break;
      case 'system.notice':
        if (data.level === 'error') toast(data.text, 'error');
        break;
      default:
        break;
    }
    queueRender();
  }

  function applyRuntime(runtime) {
    const actor = runtime.agent;
    if (state.snapshot.participants && state.snapshot.participants[actor]) {
      state.snapshot.participants[actor].last_activity = runtime.created_at || new Date().toISOString();
    }
    if (runtime.kind === 'turn.started') state.drafts[actor] = '';
    if (runtime.kind === 'text.delta') state.drafts[actor] = (state.drafts[actor] || '') + (runtime.text || '');
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
    const nearBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 140;
    $('room-name').textContent = state.snapshot.meta.name;
    $('repo-path').textContent = state.snapshot.meta.repo;
    renderParticipants();
    renderSettings();
    renderTimeline();
    renderActivity();
    renderApprovals();
    if (forceBottom || nearBottom) requestAnimationFrame(scrollBottom);
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

      const meta = document.createElement('div');
      meta.className = 'participant-meta';
      const status = document.createElement('span');
      status.className = `state-badge state-${p.state}`;
      status.textContent = stateText(p.state);
      const model = document.createElement('span');
      model.className = 'participant-subtitle';
      model.textContent = p.model || 'native default';
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
        warning.textContent = '长时间无运行事件；可能在执行长命令、等待隐藏提示或已停滞';
        main.appendChild(warning);
      }
      if (runtime.warnings && runtime.warnings.length) {
        const warning = document.createElement('div');
        warning.className = 'runtime-line runtime-warning';
        warning.textContent = truncate(runtime.warnings.join(' · '), 160);
        warning.title = runtime.warnings.join('\n');
        main.appendChild(warning);
      }

      const roleSelect = document.createElement('select');
      roleSelect.className = 'role-select';
      roleSelect.dataset.roleActor = actor;
      [['driver', 'Driver · 实现'], ['reviewer', 'Reviewer · 独立审查'], ['peer', 'Peer · 平级讨论']].forEach(([value, label]) => {
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
        actions.appendChild(actionButton(actor, 'start', '启动'));
      } else {
        actions.appendChild(actionButton(actor, 'interrupt', '打断'));
        actions.appendChild(actionButton(actor, 'restart', '重启'));
        actions.appendChild(actionButton(actor, 'stop', '停止', true));
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
    $('routing-mode').value = state.snapshot.settings.routing_mode;
    $('max-hops').value = state.snapshot.settings.max_agent_hops;
    $('max-hops-value').value = state.snapshot.settings.max_agent_hops;
    const stall = Number(state.snapshot.settings.stall_warning_seconds ?? 300);
    $('stall-disabled').checked = stall < 0;
    $('stall-warning').disabled = stall < 0;
    $('stall-warning').value = stall < 0 ? 300 : stall;
  }

  function renderTimeline() {
    timeline.replaceChildren();
    const items = [];
    for (const message of state.snapshot.messages || []) items.push({ seq: message.seq, type: 'message', value: message });
    for (const event of state.snapshot.events || []) {
      if (event.kind === 'system.notice') items.push({ seq: event.seq, type: 'notice', value: event.data });
    }
    items.sort((a, b) => a.seq - b.seq);

    if (!items.length && !state.drafts.claude && !state.drafts.codex) {
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
    for (const item of items) {
      if (item.type === 'notice') {
        const visible = !query || String(item.value.text || '').toLocaleLowerCase().includes(query);
        const node = noticeNode(item.value);
        if (!visible) node.classList.add('search-hidden');
        else visibleCount += 1;
        timeline.appendChild(node);
      } else {
        const visible = !query || `${displayName(item.value.from)} ${item.value.text}`.toLocaleLowerCase().includes(query);
        const node = messageNode(item.value);
        if (!visible) node.classList.add('search-hidden');
        else visibleCount += 1;
        timeline.appendChild(node);
      }
    }
    ['claude', 'codex'].forEach((actor) => {
      const text = state.drafts[actor];
      if (text) timeline.appendChild(draftNode(actor, text));
    });
    if (query && visibleCount === 0) {
      const empty = document.createElement('div');
      empty.className = 'timeline-empty';
      empty.innerHTML = '<div><h2>没有匹配消息</h2><p>修改搜索关键词即可恢复完整时间线。</p></div>';
      timeline.appendChild(empty);
    }
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
    const actions = document.createElement('span');
    actions.className = 'message-actions';
    const inspect = document.createElement('button');
    inspect.className = 'reply-action inspect-action';
    inspect.textContent = '过程';
    inspect.dataset.inspectId = message.id;
    const reply = document.createElement('button');
    reply.className = 'reply-action';
    reply.textContent = '回复';
    reply.dataset.replyId = message.id;
    actions.append(inspect, reply);
    meta.append(author, time);
    if (message.retry_of) {
      const retryMarker = document.createElement('span');
      retryMarker.className = 'retry-marker';
      retryMarker.textContent = '重试';
      retryMarker.title = `Retry of ${message.retry_of}`;
      meta.appendChild(retryMarker);
    }
    meta.appendChild(actions);

    const bubble = document.createElement('div');
    bubble.className = 'message-bubble';
    if (message.reply_to) {
      const parent = state.snapshot.messages.find((item) => item.id === message.reply_to);
      if (parent) {
        const quote = document.createElement('div');
        quote.className = 'reply-quote';
        quote.textContent = `${displayName(parent.from)}：${truncate(parent.text, 110)}`;
        bubble.appendChild(quote);
      }
    }
    appendRichText(bubble, message.text);
    content.append(meta, bubble);

    if (message.delivery) {
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
        chip.title = [deliveryDetail, processingDetail].filter(Boolean).join('\n');
        delivery.appendChild(chip);
        if (isRetryable(message, target)) {
          const retryButton = document.createElement('button');
          retryButton.className = 'retry-button';
          retryButton.dataset.retryId = message.id;
          retryButton.dataset.retryTarget = target;
          retryButton.textContent = `重试 ${displayName(target)}`;
          delivery.appendChild(retryButton);
        }
      }
      content.appendChild(delivery);
    }
    row.append(avatar, content);
    return row;
  }

  function draftNode(actor, text) {
    const row = document.createElement('article');
    row.className = `message-row ${actor} streaming`;
    const avatar = document.createElement('div');
    avatar.className = `message-avatar avatar-${actor}`;
    avatar.textContent = actor === 'claude' ? 'C' : 'X';
    const content = document.createElement('div');
    content.className = 'message-content';
    const meta = document.createElement('div');
    meta.className = 'message-meta';
    meta.innerHTML = `<span class="message-author">${displayName(actor)}</span><span>正在输入</span>`;
    const bubble = document.createElement('div');
    bubble.className = 'message-bubble';
    appendRichText(bubble, text);
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

  function appendRichText(parent, text) {
    const parts = String(text || '').split(/(@(?:claude|codex|all|peer|human|user)\b)/gi);
    parts.forEach((part) => {
      if (/^@(?:claude|codex|all|peer|human|user)$/i.test(part)) {
        const span = document.createElement('span');
        span.className = 'mention';
        span.textContent = part;
        parent.appendChild(span);
      } else {
        const lines = part.split('\n');
        lines.forEach((line, index) => {
          if (index) parent.appendChild(document.createElement('br'));
          parent.appendChild(document.createTextNode(line));
        });
      }
    });
  }

  function renderActivity() {
    const container = $('activity-tab');
    container.replaceChildren();
    const scope = $('inspector-scope');
    const scopedMessage = state.inspectorCorrelation
      ? (state.snapshot.messages || []).find((message) => message.id === state.inspectorCorrelation)
      : null;
    const turns = new Set(scopedMessage ? Object.values(scopedMessage.processing_turn || {}).filter(Boolean) : []);
    if (scopedMessage) {
      scope.classList.remove('hidden');
      $('inspector-scope-text').textContent = `仅显示 ${displayName(scopedMessage.from)} 消息 ${truncate(scopedMessage.id, 18)} 的工作过程`;
    } else {
      scope.classList.add('hidden');
      $('inspector-scope-text').textContent = '';
    }
    const events = (state.snapshot.events || [])
      .filter((event) => event.kind === 'runtime.event')
      .map((event) => ({ seq: event.seq, ...event.data }))
      .filter((event) => state.inspectorAgent === 'all' || event.agent === state.inspectorAgent)
      .filter((event) => !['text.delta', 'state', 'session'].includes(event.kind))
      .filter((event) => !scopedMessage || event.correlation_id === scopedMessage.id || (event.turn_id && turns.has(event.turn_id)))
      .slice(-100)
      .reverse();
    if (!events.length) {
      const empty = document.createElement('div');
      empty.className = 'activity-empty';
      empty.textContent = scopedMessage
        ? '该消息暂时没有可重放的持久运行事件；高频流式文本和命令增量仅在实时连接期间展示。'
        : 'Agent 的工具调用、命令、计划、Diff 和运行日志会显示在这里。';
      container.appendChild(empty);
      return;
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
  }

  function renderApprovals() {
    const container = $('approvals-tab');
    container.replaceChildren();
    const pending = (state.snapshot.approvals || []).filter((item) => item.status === 'pending');
    $('approval-count').textContent = String(pending.length);
    if (!pending.length) {
      const empty = document.createElement('div');
      empty.className = 'approvals-empty';
      empty.textContent = '当前没有待处理审批。Codex 的命令和文件变更请求会在这里等待你的决定。';
      container.appendChild(empty);
      return;
    }
    pending.forEach((approval) => {
      const card = document.createElement('div');
      card.className = 'approval-card';
      const title = document.createElement('div');
      title.className = 'approval-title';
      title.textContent = approval.title;
      const meta = document.createElement('div');
      meta.className = 'approval-meta';
      meta.textContent = `${displayName(approval.agent)} · ${approval.kind}`;
      const detail = document.createElement('pre');
      detail.className = 'approval-detail';
      detail.textContent = prettyJSON(approval.detail);
      const actions = document.createElement('div');
      actions.className = 'approval-actions';
      actions.append(
        approvalButton(approval.id, 'accept', '允许一次', 'approve-button'),
        approvalButton(approval.id, 'acceptForSession', '本会话允许', 'approve-button'),
        approvalButton(approval.id, 'decline', '拒绝', 'decline-button'),
      );
      card.append(title, meta, detail, actions);
      container.appendChild(card);
    });
  }

  function approvalButton(id, decision, label, className) {
    const button = document.createElement('button');
    button.className = className;
    button.dataset.approvalId = id;
    button.dataset.decision = decision;
    button.textContent = label;
    return button;
  }

  async function sendMessage() {
    const text = messageInput.value.trim();
    if (!text) return;
    const targetMap = { all: ['claude', 'codex'], claude: ['claude'], codex: ['codex'] };
    $('send-button').disabled = true;
    try {
      await api('/api/v1/messages', {
        method: 'POST',
        body: JSON.stringify({ text, to: targetMap[state.selectedTarget], reply_to: state.replyTo || undefined }),
      });
      messageInput.value = '';
      clearReply();
      autoSizeComposer();
      scrollBottom();
    } catch (error) {
      toast(error.message, 'error');
    } finally {
      $('send-button').disabled = false;
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
          routing_mode: $('routing-mode').value,
          max_agent_hops: Number($('max-hops').value),
          stall_warning_seconds: $('stall-disabled').checked ? -1 : Number($('stall-warning').value),
        }),
      });
      toast('讨论策略已保存', 'success');
    } catch (error) {
      toast(error.message, 'error');
    }
  }

  async function retryMessage(messageId, target, button) {
    button.disabled = true;
    const old = button.textContent;
    button.textContent = '重试中…';
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

  async function downloadExport(format) {
    try {
      const headers = new Headers();
      if (token) headers.set('Authorization', `Bearer ${token}`);
      const response = await fetch(`/api/v1/export?format=${encodeURIComponent(format)}`, { headers });
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

  async function resolveApproval(id, decision, button) {
    button.disabled = true;
    try {
      await api(`/api/v1/approvals/${id}`, { method: 'POST', body: JSON.stringify({ decision }) });
      toast('审批决定已提交', 'success');
    } catch (error) {
      toast(error.message, 'error');
      button.disabled = false;
    }
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
    $('diff-output').textContent = '读取 Diff…';
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
    $('reply-preview').textContent = `${displayName(message.from)}：${truncate(message.text, 120)}`;
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

  function setTarget(target) {
    state.selectedTarget = target;
    document.querySelectorAll('.target-button').forEach((button) => button.classList.toggle('active', button.dataset.target === target));
    const labels = { all: '发送给 Claude 与 Codex', claude: '仅发送给 Claude', codex: '仅发送给 Codex' };
    $('delivery-hint').textContent = labels[target];
    messageInput.focus();
  }

  function setConnection(connected, label) {
    const node = $('connection');
    node.classList.toggle('connected', connected);
    node.classList.toggle('disconnected', !connected);
    node.lastElementChild.textContent = label;
  }

  function autoSizeComposer() {
    messageInput.style.height = 'auto';
    messageInput.style.height = `${Math.min(messageInput.scrollHeight, 150)}px`;
  }

  function scrollBottom() { timeline.scrollTop = timeline.scrollHeight; }

  function toast(message, type = '') {
    const node = document.createElement('div');
    node.className = `toast ${type}`;
    node.textContent = message;
    $('toast-stack').appendChild(node);
    setTimeout(() => node.remove(), 4300);
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
    return ({ pending: '发送中', started: '已开始新 Turn', injected: '已注入当前 Turn', queued: '已排队', failed: '失败', skipped: '已跳过' })[value] || value;
  }
  function processingText(value) {
    return ({ waiting: '等待处理', working: '处理中', completed: '已完成', cancelled: '已取消', failed: '处理失败', superseded: '已被新指令取代' })[value] || value;
  }
  function isRetryable(message, target) {
    const processing = message.processing && message.processing[target];
    if (['failed', 'cancelled', 'superseded'].includes(processing)) return true;
    const delivery = message.delivery && message.delivery[target];
    return ['failed', 'skipped'].includes(delivery);
  }
  function actionText(value) {
    return ({ start: '已启动', stop: '已停止', restart: '已重启', interrupt: '已请求打断' })[value] || value;
  }
  function sessionSummary(p) {
    if (p.current_turn) return `Turn ${truncate(p.current_turn, 18)}`;
    if (p.session_id) return `Session ${truncate(p.session_id, 18)}`;
    return '等待启动原生 Agent';
  }
  function formatTime(value) {
    try { return new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(value)); }
    catch { return ''; }
  }
  function truncate(value, length) {
    const text = String(value || '').replace(/\s+/g, ' ');
    return text.length > length ? `${text.slice(0, length)}…` : text;
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
    const targetButton = event.target.closest('.target-button');
    if (targetButton) setTarget(targetButton.dataset.target);
    const action = event.target.closest('[data-action][data-actor]');
    if (action) participantAction(action.dataset.actor, action.dataset.action, action);
    const reply = event.target.closest('[data-reply-id]');
    if (reply) setReply(reply.dataset.replyId);
    const inspect = event.target.closest('[data-inspect-id]');
    if (inspect) inspectMessage(inspect.dataset.inspectId);
    const tab = event.target.closest('.tab');
    if (tab) switchTab(tab.dataset.tab);
    const approval = event.target.closest('[data-approval-id][data-decision]');
    if (approval) resolveApproval(approval.dataset.approvalId, approval.dataset.decision, approval);
    const retry = event.target.closest('[data-retry-id][data-retry-target]');
    if (retry) retryMessage(retry.dataset.retryId, retry.dataset.retryTarget, retry);
  });
  document.addEventListener('change', (event) => {
    if (event.target.matches('[data-role-actor]')) setRole(event.target.dataset.roleActor, event.target.value);
  });
  $('send-button').addEventListener('click', sendMessage);
  messageInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      sendMessage();
    }
  });
  messageInput.addEventListener('input', autoSizeComposer);
  $('cancel-reply').addEventListener('click', clearReply);
  $('save-settings').addEventListener('click', saveSettings);
  $('max-hops').addEventListener('input', () => { $('max-hops-value').value = $('max-hops').value; });
  $('stall-disabled').addEventListener('change', () => { $('stall-warning').disabled = $('stall-disabled').checked; });
  $('refresh-button').addEventListener('click', loadSnapshot);
  $('message-search').addEventListener('input', (event) => { state.searchQuery = event.target.value; queueRender(); });
  $('export-markdown').addEventListener('click', () => downloadExport('markdown'));
  $('export-json').addEventListener('click', () => downloadExport('json'));
  $('theme-button').addEventListener('click', () => {
    state.theme = state.theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = state.theme;
    localStorage.setItem('pairroom.theme', state.theme);
  });
  $('scroll-bottom').addEventListener('click', scrollBottom);
  $('refresh-diff').addEventListener('click', refreshDiff);
  $('inspector-agent').addEventListener('change', (event) => { state.inspectorAgent = event.target.value; renderActivity(); });
  $('clear-inspector-scope').addEventListener('click', clearInspectorScope);
  document.addEventListener('keydown', (event) => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLocaleLowerCase() === 'k') {
      event.preventDefault();
      $('message-search').focus();
    }
    if (event.key === 'Escape') {
      if (state.replyTo) clearReply();
      else if (state.inspectorCorrelation) clearInspectorScope();
      else if (state.searchQuery) {
        state.searchQuery = '';
        $('message-search').value = '';
        queueRender();
      }
    }
  });

  loadSnapshot().catch((error) => {
    toast(error.message, 'error');
    setConnection(false, 'Offline');
  });
})();
