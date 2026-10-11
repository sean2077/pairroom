/* Local-owner LAN controls. Joined Rooms project CLI-owned state; private keys never enter the browser. */
(() => {
  'use strict';
  function create({ t, node, actionButton, api, settingsPanel, settingRow, copyText, showDialog, closeDialog,
    confirm, refresh, renderSettings, getSnapshot, isSettings, isCurrent }) {
    const $ = id => document.getElementById(id);
    let revision = 0, configuration = null, loading = false, configError = '';
    let dialogRevision = 0, currentRoom = null;
    const outbox = () => window.PairRoomNativeOutbox;
    async function call(path, method = 'GET', body) {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 30000);
      try { return await api(path, { method, ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: controller.signal }); }
      finally { clearTimeout(timer); }
    }
    function reset() {
      revision++; dialogRevision++; configuration = null; loading = false; configError = ''; currentRoom = null;
      if ($('lan-room-dialog')?.open) closeDialog('lan-room-dialog');
      $('lan-room-body')?.replaceChildren();
    }
    function errorText(error) {
      if (error.name === 'AbortError') return t('room.native.requestTimeout');
      if (error.message === 'outbox_busy') return t('room.native.outboxBusy');
      if (error.message === 'outbox_conflict') return t('room.native.savedDraft');
      if (['outbox_invalid', 'outbox_unavailable'].includes(error.message)) return t('room.native.storageFailed');
      return error.message;
    }
    async function loadConfiguration() {
      if (loading) return;
      const ticket = revision, active = isCurrent();
      loading = true; configError = '';
      try {
        const value = await call('/api/v1/lan');
        if (ticket === revision && active()) configuration = value;
      } catch (error) { if (ticket === revision && active()) configError = errorText(error); }
      finally {
        if (ticket === revision && active()) { loading = false; if (isSettings()) renderSettings(); }
      }
    }
    async function ensureEnabled() {
      const value = await call('/api/v1/lan');
      if (!value.enabled) throw new Error(t('room.lan.enableFirst'));
    }
    function settings() {
      if (!configuration && !loading && !configError) loadConfiguration();
      const enabled = configuration?.enabled === true;
      const address = node('input', { id: 'lan-listen-address', type: 'text', value: configuration?.address || '',
        placeholder: '192.168.1.23:8877', 'aria-label': t('room.lan.address'), autocomplete: 'off', spellcheck: 'false', disabled: loading || !configuration });
      const enabledInput = node('input', { id: 'lan-listen-enabled', type: 'checkbox', checked: enabled, disabled: loading || !configuration });
      const notice = node('p', { role: 'status', className: 'field-help', textContent: configError || (loading ? t('ui.processing') : '') });
      const save = actionButton(t('ui.saveSettings'), async () => {
        const ticket = revision, active = isCurrent();
        if (loading) return;
        loading = true; save.disabled = true; address.disabled = true; enabledInput.disabled = true;
        try {
          const value = await call('/api/v1/lan', 'PUT', { enabled: enabledInput.checked, address: address.value.trim() });
          if (ticket !== revision || !active()) return;
          configuration = value; configError = ''; notice.textContent = t('room.lan.saved');
        } catch (error) { if (ticket === revision && active()) configError = errorText(error); }
        finally { if (ticket === revision && active()) { loading = false; if (isSettings()) renderSettings(); } }
      }, 'primary-button', loading || !configuration);
      const rows = [
        settingRow(t('room.lan.enable'), t('room.lan.enableHelp'), node('label', { className: 'checkbox-label' }, enabledInput, node('span', { textContent: t(enabled ? 'room.lan.enabled' : 'room.lan.disabled') }))),
        settingRow(t('room.lan.address'), t('room.lan.addressHelp'), address),
        node('div', { className: 'lan-settings-actions' }, save,
          actionButton(t('ui.refresh'), loadConfiguration, 'secondary-button', loading), notice),
      ];
      if (configuration?.endpoint) rows.push(settingRow(t('room.lan.endpoint'), '', node('code', { className: 'lan-identifier', textContent: configuration.endpoint })));
      if (configuration?.host_pin) rows.push(settingRow(t('room.lan.hostPin'), t('room.lan.pinHelp'), node('code', { className: 'lan-identifier', textContent: configuration.host_pin })));
      if (configuration?.diagnostic) rows.push(node('p', { role: 'status', className: 'field-help', textContent: configuration.diagnostic }));
      return node('div', { className: 'view-stack' }, settingsPanel(t('room.lan.settings'), t('room.lan.boundary'), ...rows), joinedList(true));
    }
    function joinedList(showEmpty = false) {
      const rooms = getSnapshot()?.joined_rooms || [];
      if (!rooms.length && !showEmpty) return null;
      return settingsPanel(t('room.lan.joinedRooms'), t('room.lan.joinedHelp'),
        ...(rooms.length ? rooms.map(room => settingRow(room.name || room.remote_room_id,
          `${t(`room.lan.status.${room.status}`)} · ${t(room.connected ? 'room.lan.connected' : 'room.lan.disconnected')}`,
          actionButton(t('ui.open'), () => openJoined(room), 'secondary-button compact-button')))
          : [node('p', { className: 'field-help', textContent: t('room.lan.noJoinedRooms') })]));
    }
    function dialogTicket() {
      const value = ++dialogRevision, active = isCurrent();
      return () => value === dialogRevision && active() && $('lan-room-dialog').open;
    }
    function lastContact(room) {
      const observed = Date.parse(room.last_seen || '');
      return Number.isFinite(observed) && observed > 0 ? new Date(observed).toISOString() : t('room.lan.notObserved');
    }
    function section(title, ...content) {
      return node('section', { className: 'lan-dialog-section' }, node('h3', { textContent: title }), ...content);
    }
    function field(label, value) {
      return node('p', { className: 'field-help' }, node('strong', { textContent: `${label}: ` }), node('code', { className: 'lan-identifier', textContent: String(value || '—') }));
    }
    async function openRoom(room) {
      currentRoom = room;
      $('lan-room-title').textContent = `${t('room.lan.roomAccess')} · ${room.name}`;
      $('lan-room-body').replaceChildren(node('p', { textContent: t('ui.processing') }));
      showDialog('lan-room-dialog');
      const active = dialogTicket();
      try {
        const value = await call(`/api/v1/rooms/${encodeURIComponent(room.id)}/lan`);
        if (active()) renderRoomAccess(room, value, active);
      } catch (error) {
        if (active()) $('lan-room-body').replaceChildren(node('p', { role: 'alert', textContent: errorText(error) }),
          actionButton(t('ui.retryNow'), () => openRoom(room), 'secondary-button'));
      }
    }
    function renderRoomAccess(room, value, active) {
      const prefix = `/api/v1/rooms/${encodeURIComponent(room.id)}/lan`;
      const member = value.member?.binding;
      const notice = node('p', { className: 'field-help', role: 'status' });
      const inviteOutput = node('textarea', { id: 'lan-invite-output', rows: '3', readOnly: true, 'aria-label': t('room.lan.inviteCommand'), hidden: true });
      const copy = actionButton(t('ui.copy'), () => copyText(inviteOutput.value), 'secondary-button compact-button'); copy.hidden = true;
      let busy = false;
      const invite = actionButton(t('room.lan.createInvite'), async () => {
        if (busy) return;
        busy = true; invite.disabled = true;
        try {
          const created = await call(`${prefix}/invite`, 'POST', {});
          if (!active()) return;
          if (!/^pairroom:\/\/join\/[A-Za-z0-9_-]+$/.test(created.invite)) throw new Error(t('room.lan.invalidInvite'));
          inviteOutput.value = `pairroom relay join '${created.invite}'`; inviteOutput.hidden = false; copy.hidden = false;
          notice.textContent = t('room.lan.inviteExpires', { value: new Date(created.expires_at).toLocaleString() });
        } catch (error) { if (active()) notice.textContent = errorText(error); }
        finally { busy = false; if (active()) invite.disabled = false; }
      }, 'primary-button', Boolean(member?.active));
      const pending = (value.pending || []).map(request => node('article', { className: 'lan-pending-request' },
        node('strong', { textContent: request.label || t('room.lan.joinRequest') }),
        field(t('room.lan.requestID'), request.request_id), field(t('room.lan.peerPin'), request.fingerprint), field(t('agent.runtime'), request.runtime)));
      const receipt = node('textarea', { id: 'lan-accept-receipt', rows: '3', maxLength: '2048', spellcheck: 'false', autocomplete: 'off', 'aria-label': t('room.lan.trustedReceipt') });
      const accept = actionButton(t('room.lan.accept'), async () => {
        if (busy || !receipt.value.trim()) return;
        const exact = receipt.value.trim();
        if (!/^pairroom-accept:[A-Za-z0-9_-]+$/.test(exact)) { notice.textContent = t('room.lan.invalidReceipt'); return; }
        busy = true; accept.disabled = true; receipt.disabled = true;
        try {
          await call(`${prefix}/accept`, 'POST', { receipt: exact });
          if (!active()) return;
          await refresh({ forceRender: true, fresh: true });
          if (active()) await openRoom(room);
        } catch (error) { if (active()) notice.textContent = errorText(error); }
        finally { busy = false; if (active()) { accept.disabled = false; receipt.disabled = false; } }
      }, 'primary-button', Boolean(member?.active));
      const memberSection = member?.active ? section(t('room.lan.member'),
        field(t('agent.runtime'), member.runtime), field(t('room.lan.peerPin'), member.remote_key), field(t('room.native.generation'), member.generation),
        actionButton(t('room.lan.revoke'), () => confirm({ title: t('room.lan.revoke'), message: t('room.lan.revokeHelp'), label: t('room.lan.revoke'), tone: 'danger',
          action: async () => { await call(`${prefix}/revoke`, 'POST', {}); await refresh({ forceRender: true, fresh: true }); if (active()) await openRoom(room); } }), 'danger-button outline'))
        : section(t('room.lan.pending'), node('p', { className: 'field-help', textContent: t('room.lan.receiptHelp') }),
          ...pending, pending.length ? null : node('p', { className: 'field-help', textContent: t('room.lan.noPending') }),
          node('label', { className: 'field-label', for: 'lan-accept-receipt', textContent: t('room.lan.trustedReceipt') }), receipt, accept);
      $('lan-room-body').replaceChildren(
        section(t('room.lan.invite'), node('p', { className: 'field-help', textContent: t('room.lan.inviteHelp') }), invite, inviteOutput, copy),
        memberSection, notice, actionButton(t('ui.refresh'), () => openRoom(room), 'secondary-button'));
    }
    function messageAuthor(room, message) {
      if (message.from !== 'user') return message.from === 'slot1' ? t('agent.agent1') : t('agent.agent2');
      if (message.author === 'host_owner') return t('room.lan.hostHuman');
      // This dialog is the joined member's own view: a "lan:" author names this
      // member's identity, so compare the pinned key instead of assuming the
      // other side wrote it. An author this view cannot attribute stays neutral.
      if (room.owner_key && message.author === `lan:${room.owner_key}`) return t('room.lan.localHuman');
      return t('room.lan.remoteHuman');
    }
    function messageCard(room, message) {
      const attachments = (message.attachments || []).map(item => node('a', {
        href: `/api/v1/lan/joined/${encodeURIComponent(room.id)}/attachments/${encodeURIComponent(item.id)}`,
        download: item.name || item.id, className: 'lan-artifact', textContent: `${item.name || item.id} · ${item.size || 0} B`,
      }));
      return node('article', { className: 'lan-shared-message', 'data-message-id': message.id },
        node('header', {}, node('strong', { textContent: messageAuthor(room, message) }),
          node('span', { textContent: ` → ${message.to === 'user' ? '@user' : message.to === 'slot1' ? t('agent.agent1') : t('agent.agent2')}` }),
          node('span', { className: 'badge plain', textContent: t(`room.native.${message.state}`) })),
        message.quote ? node('blockquote', { textContent: message.quote.text || '' }) : null,
        node('pre', { textContent: message.text }), ...attachments);
    }
    function openJoined(room) {
      currentRoom = room;
      $('lan-room-title').textContent = room.name || room.remote_room_id;
      showDialog('lan-room-dialog');
      const active = dialogTicket(), prefix = `/api/v1/lan/joined/${encodeURIComponent(room.id)}`;
      let busy = false, cursor = '', nextCursor = '';
      const notice = node('p', { role: 'status', className: 'field-help' });
      const history = node('div', { className: 'lan-shared-history', role: 'log', 'aria-label': t('room.native.messages') });
      const target = node('select', { 'aria-label': t('room.native.target') },
        node('option', { value: 'slot1', textContent: t('agent.agent1') }), node('option', { value: 'slot2', textContent: t('agent.agent2') }));
      const text = node('textarea', { rows: '4', 'aria-label': t('room.native.message'), maxLength: '262144' });
      const pendingNotice = node('p', { className: 'field-help' });
      let saved = null, storageError = false;
      function loadDraft() {
        try { saved = outbox().load(localStorage, room.id); storageError = false; }
        catch { storageError = true; pendingNotice.textContent = t('room.native.storageFailed'); }
        if (saved) { text.value = saved.text; target.value = saved.to; pendingNotice.textContent = t('room.native.savedDraft'); }
        else if (!storageError) pendingNotice.textContent = '';
        updateControls();
      }
      function updateControls() {
        text.disabled = target.disabled = busy || Boolean(saved) || storageError;
        send.disabled = busy || storageError || room.status !== 'accepted';
        send.textContent = t(saved ? 'room.native.retryOriginal' : 'room.native.send');
        check.hidden = forget.hidden = !saved && !storageError;
        check.disabled = busy || !saved || storageError;
        forget.disabled = busy || (!saved && !storageError);
      }
      async function clearAccepted(payload, message) {
        if (!outbox().matches(payload, message)) throw new Error(t('room.native.receiptConflict'));
        await outbox().clear(localStorage, room.id, payload.id);
        if (active()) { saved = null; text.value = ''; pendingNotice.textContent = ''; }
      }
      const send = actionButton(t('room.native.send'), async () => {
        if (busy || storageError) return;
        const payload = saved || { id: crypto.randomUUID(), text: text.value.trim(), to: target.value, attachment_ids: [] };
        if (!payload.text) return;
        busy = true; updateControls();
        try {
          await outbox().save(localStorage, room.id, payload);
          if (active()) saved = payload;
          const message = await call(`${prefix}/send`, 'POST', payload);
          await clearAccepted(payload, message);
          if (active()) { notice.textContent = t('room.native.sent'); cursor = ''; await loadHistory(); }
        } catch (error) { if (active()) { notice.textContent = errorText(error); loadDraft(); } }
        finally { busy = false; if (active()) updateControls(); }
      }, 'primary-button');
      const check = actionButton(t('room.native.checkReceipt'), async () => {
        if (busy || !saved) return;
        const payload = saved; busy = true; updateControls();
        try {
          const receipt = await call(`${prefix}/receipt`, 'POST', { id: payload.id });
          if (receipt.accepted) { await clearAccepted(payload, receipt.message); if (active()) notice.textContent = t('room.native.receiptRecovered'); }
          else if (active()) notice.textContent = t('room.native.notFoundReceipt');
        } catch (error) { if (active()) notice.textContent = errorText(error); }
        finally { busy = false; if (active()) updateControls(); }
      }, 'secondary-button');
      const forget = actionButton(t('room.native.forgetDraft'), () => {
        let captured; try { captured = outbox().capture(localStorage, room.id); } catch { return; }
        confirm({ title: t('room.native.forgetTitle'), message: t('room.native.forgetBody'), label: t('room.native.forgetDraft'), tone: 'danger',
          action: async () => { await outbox().forget(localStorage, room.id, captured); if (active()) { saved = null; storageError = false; text.value = ''; loadDraft(); } } });
      }, 'secondary-button');
      const next = actionButton(t('room.native.nextPage'), () => { cursor = nextCursor; return loadHistory(); }, 'secondary-button'); next.hidden = true;
      let reading = false;
      async function loadHistory() {
        if (reading) return;
        reading = true; next.disabled = true;
        try {
          const page = await call(`${prefix}/history`, 'POST', { cursor, limit: 30 });
          if (!active()) return;
          history.replaceChildren(...(page.messages || []).map(message => messageCard(room, message)));
          if (!page.messages?.length) history.append(node('p', { className: 'field-help', textContent: t('room.native.empty') }));
          nextCursor = page.next_cursor || ''; next.hidden = !page.has_more;
        } catch (error) { if (active()) notice.textContent = errorText(error); }
        finally { reading = false; if (active()) next.disabled = false; }
      }
      const connected = t(room.connected ? 'room.lan.connected' : 'room.lan.disconnected');
      const leave = actionButton(t('room.lan.leave'), () => confirm({ title: t('room.lan.leave'), message: t('room.lan.leaveHelp'), label: t('room.lan.leave'), tone: 'danger',
        action: async () => { await call(`${prefix}/leave`, 'POST', {}); if (active()) closeDialog('lan-room-dialog'); await refresh({ forceRender: true, fresh: true }); } }), 'danger-button outline');
      leave.hidden = !['accepted', 'revoked'].includes(room.status);
      const detach = actionButton(t('room.lan.detach'), () => confirm({ title: t('room.lan.detach'), message: t('room.lan.detachHelp'), label: t('room.lan.detach'), tone: 'danger',
        action: async () => { await call(`${prefix}/detach`, 'POST', {}); if (active()) closeDialog('lan-room-dialog'); await refresh({ forceRender: true, fresh: true }); } }), 'secondary-button');
      detach.hidden = ['left', 'detached'].includes(room.status);
      $('lan-room-body').replaceChildren(
        section(t('room.lan.joinedRoom'), node('p', { className: 'field-help', textContent: `${t(`room.lan.status.${room.status}`)} · ${connected}` }),
          node('p', { className: 'field-help', textContent: t('room.lan.connectionBoundary') }),
          field(t('room.lan.lastContact'), lastContact(room)),
          field(t('room.roomId'), room.id), field(t('room.lan.endpoint'), room.endpoint), field(t('room.lan.hostPin'), room.host_pin)),
        section(t('room.native.messages'), history, node('div', { className: 'lan-settings-actions' },
          actionButton(t('ui.refresh'), () => { cursor = ''; return loadHistory(); }, 'secondary-button'), next)),
        section(t('room.native.message'), target, text, pendingNotice, node('div', { className: 'lan-settings-actions' }, send, check, forget), notice),
        node('div', { className: 'lan-settings-actions' }, leave, detach));
      loadDraft();
      if (room.status === 'accepted') loadHistory();
    }
    return { reset, settings, joinedList, openRoom, openJoined, ensureEnabled, currentRoom: () => currentRoom };
  }
  window.PairRoomLAN = { create };
})();
