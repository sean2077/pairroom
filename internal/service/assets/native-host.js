(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const tr = key => window.PairRoomI18n.t(`room.native.${key}`);
  let csrf = '', snapshot = null, stream = null, pendingSend = null, refreshing = false, refreshAgain = false, sending = false;
  const messageNodes = new Map();
  let bindingsKey = '', auditKey = '', attentionKey = '', activityTimer = null, messagesRendered = false;
  let outboxRoom = '', outboxBroken = false, pendingCursor = '', pendingNext = '', pendingKey = '', pendingRequest = 0;
  let historyNext = '', historyFilter = '', historyRequest = 0;
  const outbox = window.PairRoomNativeOutbox;
  // Local recovery failures are fixed codes, not copy: show the translated
  // storage message and keep the raw code out of the Room status line.
  const outboxCodes = new Set(['invalid_room', 'outbox_invalid', 'outbox_conflict', 'outbox_unavailable']);
  const failureText = error => outboxCodes.has(error?.message) ? tr('storageFailed') : error?.message || '';
  const language = () => window.PairRoomI18n?.lang || 'en';
  const element = (tag, text, cls) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (cls) e.className = cls; return e; };
  const time = value => value ? new Intl.DateTimeFormat(language(), {dateStyle:'short',timeStyle:'medium'}).format(new Date(value)) : tr('never');
  function status(message, error = false) { $('status').textContent = message; $('status').classList.toggle('error', error); }
  async function request(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (options.body && !(options.body instanceof FormData)) headers.set('Content-Type','application/json');
    if (csrf && options.method && options.method !== 'GET') headers.set('X-PairRoom-CSRF',csrf);
    const res = await fetch(path, {...options, headers, cache:'no-store',credentials:'same-origin'});
    if (!res.ok) { let value = {}; try {value = await res.json();} catch (_) { /* not a JSON error */ } throw new Error(value.error || `${tr('error')} (${res.status})`); }
    return res.status === 204 ? null : res.json();
  }
  function handle(slot) { return (snapshot?.identities?.[slot]?.MentionHandle || snapshot?.identities?.[slot]?.mention_handle) || (slot === 'user' ? tr('user') : slot); }
  function readingPosition() {
    const list = $('messages');
    const stick = !messagesRendered || list.scrollHeight-list.scrollTop-list.clientHeight < 70;
    const top = list.getBoundingClientRect().top;
    const anchor = stick ? null : [...list.querySelectorAll('[data-message-id]')].find(node => node.getBoundingClientRect().bottom > top);
    return {stick, anchor, top: anchor?.getBoundingClientRect().top};
  }
  function restoreReadingPosition(position) {
    const list = $('messages');
    if(position.stick)list.scrollTop=list.scrollHeight;
    else if(position.anchor?.isConnected)list.scrollTop+=position.anchor.getBoundingClientRect().top-position.top;
  }
  const compactPanels = window.matchMedia('(max-width: 1000px)');
  const panelNames = ['participants', 'inspector'];
  const desktopPanels = { participants: true, inspector: true };
  let mobilePanel = '';
  function syncPanels() {
    const position = readingPosition(), focused = document.activeElement;
    for (const name of panelNames) {
      const panel = $(`native-${name}`);
      const open = compactPanels.matches ? mobilePanel === name : desktopPanels[name];
      panel.hidden = !open;
      document.body.classList.toggle(name === 'inspector' ? 'native-details-open' : 'native-participants-open', open);
      $(`${name}-toggle`).setAttribute('aria-expanded', String(open));
      if (!open && panel.contains(focused)) $(`${name}-toggle`).focus({preventScroll:true});
    }
    document.querySelector('.conversation').inert = compactPanels.matches && Boolean(mobilePanel);
    for (const button of $('participant-summary').children) button.setAttribute('aria-expanded', String(!$('native-participants').hidden));
    restoreReadingPosition(position);
  }
  function showPanel(name, open) {
    const wasOpen = !$(`native-${name}`).hidden;
    if (compactPanels.matches) mobilePanel = open ? name : '';
    else desktopPanels[name] = open;
    syncPanels();
    // Only a transition into view moves focus: re-opening an already visible
    // panel (Inspect inside a narrow diagnostics list, or a participant chip)
    // must not pull the keyboard user out of the control they just activated.
    if (open && !wasOpen && compactPanels.matches) $(`${name}-title`).focus({preventScroll:true});
  }
  function showInspector(open) { showPanel('inspector', open); }
  function showParticipants(open) { showPanel('participants', open); }
  for (const name of panelNames) {
    $(`${name}-toggle`).addEventListener('click', () => showPanel(name, $(`native-${name}`).hidden));
    document.querySelector(`[data-close-panel="${name}"]`).addEventListener('click', () => {
      showPanel(name, false); $(`${name}-toggle`).focus({preventScroll:true});
    });
    $(`native-${name}`).addEventListener('keydown', event => {
      if (event.key === 'Escape' && !document.querySelector('dialog[open]')) {
        event.preventDefault(); event.stopPropagation();
        showPanel(name, false); $(`${name}-toggle`).focus({preventScroll:true});
      }
    });
  }
  compactPanels.addEventListener('change', () => { mobilePanel = ''; syncPanels(); });
  syncPanels();
  $('scroll-bottom').addEventListener('click', () => { $('messages').scrollTop = $('messages').scrollHeight; });
  function translations() {
    window.PairRoomI18n.apply(document);
    $('message-text').placeholder=tr('placeholder'); bindingsKey='';auditKey='';attentionKey='';pendingKey='';messageNodes.forEach(v=>{v.key='';});
    if (snapshot) { render(snapshot); renderOutbox(); }
  }
  function renderBindings(value) {
    const key=JSON.stringify([value.relay.bindings,value.room.agents,value.identities,language()]);if(key===bindingsKey)return;bindingsKey=key;
    // Activity refreshes must not collapse a binding's open command/config detail.
    const disclosures = new Set([...$('bindings').querySelectorAll('details[open]')].map(node => node.dataset.disclosure));
    const focused = document.activeElement?.dataset.nativeFocus;
    const chips = [];
    const nodes=['slot1','slot2'].map((slot,index)=>{
      const b=value.relay.bindings[slot]||{};const selection=value.room.agents[slot]||{};
      const state = tr(!b.active?'unbound':b.session_id?'bound':'pending');
      const chip = element('button', undefined, `participant-chip ${slot}`);
      chip.type = 'button'; chip.dataset.participant = slot; chip.dataset.nativeFocus = `${slot}-chip`;
      chip.setAttribute('aria-controls', 'native-participants');
      chip.setAttribute('aria-expanded', String(!$('native-participants').hidden));
      chip.title = tr('activityHelp');
      chip.append(element('span', String(index+1), 'participant-avatar'), element('strong', handle(slot)), element('span', state, 'muted'));
      chip.addEventListener('click', () => {
        showParticipants(true);
        const binding = $('bindings').querySelector(`[data-slot="${slot}"]`);
        binding?.focus();
      });
      chips.push(chip);
      const card=element('article',undefined,'binding');card.dataset.slot=slot;card.tabIndex=-1;card.dataset.nativeFocus=slot;
      const top=element('div',undefined,'binding-top');top.append(element('h3',`${index+1} · ${handle(slot)}`),element('span',state,'badge'));
      if(b.active){const park=element('button',tr(b.park_enabled?'parkOn':'parkOff'));park.type='button';park.dataset.park=slot;park.dataset.nativeFocus=`${slot}-park`;park.setAttribute('aria-pressed',String(Boolean(b.park_enabled)));park.addEventListener('click',async()=>{park.disabled=true;try{await request(`api/v1/participants/${slot}/park`,{method:'POST',body:JSON.stringify({enabled:!b.park_enabled})});status(tr('controls'));await refresh();}catch(e){status(e.message,true);}finally{park.disabled=false;}});top.append(park);}
      card.append(top);
      const label = key => window.PairRoomI18n.t(key), inherited = label('room.nativeDefault');
      const provider = selection.provider?.source === 'cc-switch'
        ? `CC Switch · ${selection.provider.app_type}/${selection.provider.profile_id}` : label('agent.nativeProvider');
      const configMeta = element('dl', undefined, 'binding-meta agent-config');
      [[label('agent.runtime'), selection.runtime || inherited], [label('agent.provider'), provider],
        [label('agent.model'), selection.model || inherited], [label('agent.effort'), selection.effort || inherited],
        [label('agent.permissionMode'), [selection.permission_mode, selection.approval_policy, selection.sandbox].filter(Boolean).join(' · ') || inherited],
        [tr('last'), time(b.last_activity)]].forEach(([key, text]) => configMeta.append(element('dt', key), element('dd', text)));
      card.append(configMeta);
      const identity = element('details'); identity.dataset.disclosure = `${slot}-identity`;
      identity.open = disclosures.has(identity.dataset.disclosure);
      identity.append(element('summary', tr('binding'))); identity.firstElementChild.dataset.nativeFocus = `${slot}-identity`;
      const meta = element('dl', undefined, 'binding-meta');
      [[tr('binding'),b.bind_id||'—'],[tr('generation'),b.generation||'—'],[tr('session'),b.session_id||'—']].forEach(([key,text])=>meta.append(element('dt',key),element('dd',String(text))));
      identity.append(meta); card.append(identity);
      const instructions=element('details');instructions.dataset.disclosure=`${slot}-commands`;instructions.open=disclosures.has(instructions.dataset.disclosure);instructions.append(element('summary',tr('commands')),element('p',tr('setup.intro'),'muted'));
      instructions.firstElementChild.dataset.nativeFocus=`${slot}-commands`;
      instructions.append(element('pre',`pairroom relay install --runtime ${selection.runtime}\npairroom relay bind --room ${value.room.id} --slot ${index+1}\npairroom relay wait --room ${value.room.id} --slot ${index+1}`));card.append(instructions);
      const config=element('details');config.dataset.disclosure=`${slot}-config`;config.open=disclosures.has(config.dataset.disclosure);config.append(element('summary',tr('metadata')),element('pre',JSON.stringify(selection,null,2)));config.firstElementChild.dataset.nativeFocus=`${slot}-config`;card.append(config);return card;
    });$('bindings').replaceChildren(...nodes);
    $('participant-summary').replaceChildren(...chips);
    for(const option of $('target').options) option.textContent=handle(option.value);
    if(focused)[...document.querySelectorAll('[data-native-focus]')].find(node=>node.dataset.nativeFocus===focused)?.focus({preventScroll:true});
  }
  async function confirmAction(title,body){$('confirm-retry').textContent=title==='retryTitle'?tr('retry'):tr('forgetDraft');$('confirm-title').textContent=tr(title);$('confirm-body').textContent=tr(body);const dialog=$('confirm-dialog');dialog.returnValue='cancel';return new Promise(resolve=>{dialog.addEventListener('close',()=>resolve(dialog.returnValue==='confirm'),{once:true});dialog.showModal();});}
  const confirmRetry=()=>confirmAction('retryTitle','retryBody');
  const MAX_RENDERED_MESSAGES=300;
  const MAX_MESSAGE_BYTES=256*1024;
  function renderMessages(value){
    const list = $('messages');
    // Measure BEFORE pruning or appending: a long incoming message must not
    // switch a reader at the bottom into history-reading mode.
    const position = readingPosition();
    const all=value.relay.messages||[];
    const total=value.relay.total_messages ?? all.length;
    // Both the HTTP projection and DOM remain bounded. Never merge the inbox
    // and audit into synthetic messages, or hide intentional duplicate sends.
    const messages=all.length>MAX_RENDERED_MESSAGES?all.slice(-MAX_RENDERED_MESSAGES):all;
    const active=new Set(messages.map(m=>m.id));
    for(const [id,v] of messageNodes){if(!active.has(id)){v.node.remove();messageNodes.delete(id);}}
    $('message-count').textContent=String(total);list.querySelector('.empty')?.remove();list.querySelectorAll('.truncated-note').forEach(n=>n.remove());
    if(!all.length){list.append(element('div',tr('empty'),'empty'));messagesRendered=false;return;}
    if(total>messages.length){list.prepend(element('div',`${tr('showingLatest')} ${messages.length} / ${total}`,'muted truncated-note'));}
    for(const m of messages){
      const key=JSON.stringify([m,language(),handle(m.from),handle(m.to)]);
      let entry=messageNodes.get(m.id);
      if(!entry){entry={node:element('article'),key:''};messageNodes.set(m.id,entry);list.append(entry.node);}
      if(key===entry.key)continue;
      entry.key=key;
      const node=entry.node;
      const actor=['user','slot1','slot2'].includes(m.from)?m.from:'other';
      node.className=`message message-row ${actor} state-${m.state}`;node.dataset.messageId=m.id;
      fillMessage(node,m,'chat');
    }
    messagesRendered=true;
    restoreReadingPosition(position);
  }
  function fillMessage(node,m,view){
    const chat=view==='chat', actor=['user','slot1','slot2'].includes(m.from)?m.from:'other';
    const head=element('div',undefined,chat?'message-meta':'message-heading');
    const stamp=element('time',time(m.created_at));stamp.dateTime=m.created_at;
    head.append(element('strong',handle(m.from),'message-author'),element('span',`${tr('target')} ${handle(m.to)}`,'message-target'),stamp);
    const bubble=element('div',undefined,chat?'message-bubble':'message-evidence');
    if(m.quote)bubble.append(element('blockquote',`${m.quote.from_handle || ''}\n${m.quote.text || ''}`,'reply-quote'));
    const body=element('div',undefined,'message-body');
    const content=()=>window.PairRoomRichText.render(body,m.text,{createImage:(_ref,alt)=>element('span',alt||tr('externalImage'),'muted'),onCopyError:()=>status(tr('copyFailed'),true)});
    if(m.text.length>3000||view==='pending'){
      const details=element('details');details.append(element('summary',m.text.slice(0,120)||tr('evidence')));let loaded=false;
      details.addEventListener('toggle',()=>{if(details.open&&!loaded){loaded=true;content();}});details.append(body);bubble.append(details);
    }else{content();bubble.append(body);}
    if(m.review){
      const evidence=element('details',undefined,'review-evidence');evidence.append(element('summary',`${tr('reviewVersion')} · ${m.review.head.slice(0,12)}`),element('pre',JSON.stringify(m.review,null,2)));
      const check=element('button',tr('checkReview'));check.type='button';check.dataset.review=m.id;
      const result=element('span',tr('reviewUnverified'),'muted');check.addEventListener('click',async()=>{check.disabled=true;try{const value=await request(`api/v1/review?id=${encodeURIComponent(m.id)}`);result.textContent=tr(`review_${value.status}`);}catch(e){status(e.message,true);}finally{check.disabled=false;}});
      evidence.append(check,result);bubble.append(evidence);
    }
    for(const a of m.attachments||[]){const img=element('img');img.alt=a.name||tr('attach');img.loading='lazy';img.src=`api/v1/attachments/${encodeURIComponent(a.id)}`;bubble.append(img);}
    const footer=element('div',undefined,'message-footer');footer.append(element('span',tr(m.state),'badge'));
    const inspect=element('button',tr('inspect'),'message-action');inspect.type='button';inspect.addEventListener('click',()=>inspectMessage(m.id));footer.append(inspect);
    if(m.state==='queued'||m.state==='unknown'){
      const action=m.state==='queued'?'cancel':'retry';const button=element('button',tr(action),'message-action');button.type='button';button.dataset.action=action;
      button.addEventListener('click',async()=>{if(action==='retry'&&!await confirmRetry())return;button.disabled=true;try{await request(`api/v1/messages/${encodeURIComponent(m.id)}/${action}`,{method:'POST',body:'{}'});pendingKey='';await refresh();}catch(e){status(e.message,true);}finally{button.disabled=false;}});footer.append(button);
    }
    if(chat){
      const avatar=element('div',actor==='user'?'Y':actor==='slot1'?'1':actor==='slot2'?'2':'·','message-avatar');avatar.setAttribute('aria-hidden','true');
      const content=element('div',undefined,'message-content');content.append(head,bubble,footer);node.replaceChildren(avatar,content);
    }else node.replaceChildren(head,bubble,footer);
  }
  function pageMessages(container,messages,view){
    container.replaceChildren(...messages.map(m=>{const node=element('article',undefined,`message state-${m.state}`);node.dataset[view==='pending'?'pendingId':'historyId']=m.id;fillMessage(node,m,view);return node;}));
    if(!messages.length)container.append(element('p',tr('noItems'),'muted'));
  }
  async function refreshPending(){
    const key=JSON.stringify([snapshot?.relay.sequence,pendingCursor,language()]);if(key===pendingKey)return;pendingKey=key;
    const serial=++pendingRequest;
    try{
      const page=await request(`api/v1/pending?limit=10${pendingCursor?`&cursor=${encodeURIComponent(pendingCursor)}`:''}`);
      if(serial!==pendingRequest)return;
      pendingNext=page.next_cursor||'';$('pending-count').textContent=String(page.total);$('pending-next').disabled=!pendingNext;$('pending-first').disabled=!pendingCursor;
      pageMessages($('pending-items'),page.messages,'pending');
    }catch(e){if(serial===pendingRequest)pendingKey='';throw e;}
  }
  $('pending-first').addEventListener('click',()=>{pendingCursor='';pendingKey='';refreshPending().catch(e=>status(e.message,true));});
  $('pending-next').addEventListener('click',()=>{pendingCursor=pendingNext;refreshPending().catch(e=>status(e.message,true));});
  async function loadHistory(cursor=''){
    const serial=++historyRequest;
    const page=await request(`api/v1/history?limit=20${historyFilter}${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);
    if(serial!==historyRequest)return;
    historyNext=page.next_cursor||'';$('history-next').hidden=!historyNext;pageMessages($('history-items'),page.messages,'history');
  }
  function inspectMessage(id){showInspector(true);$('history-panel').open=true;$('history-id').value=id;$('history-since').value='';historyFilter=`&id=${encodeURIComponent(id)}`;loadHistory().then(()=>$('history-panel').scrollIntoView({block:'nearest'})).catch(e=>status(e.message,true));}
  $('history-form').addEventListener('submit',event=>{
    event.preventDefault();const id=$('history-id').value.trim(),since=$('history-since').value;
    if(id&&since){status(tr('chooseHistoryFilter'),true);return;}
    historyFilter=id?`&id=${encodeURIComponent(id)}`:since?`&since=${encodeURIComponent(new Date(since).toISOString())}`:'';
    loadHistory().catch(e=>status(e.message,true));
  });
  $('history-next').addEventListener('click',()=>loadHistory(historyNext).catch(e=>status(e.message,true)));
  $('diagnose').addEventListener('click',async()=>{
    $('diagnose').disabled=true;try{
      const report=await request('api/v1/diagnostics');
      $('diagnostics').replaceChildren(element('p',tr('diagnosticBoundary'),'muted'),...Object.entries(report.participants).map(([slot,d])=>{
        const card=element('section');card.append(element('h3',handle(slot)),element('p',`${tr(`reason_${d.reason}`)} · ${tr(`cap_${d.capability}`)}`),element('p',tr(`next_${d.next_action}`)));
        if(d.next_eligible_at)card.append(element('p',`${tr('nextEligible')} ${time(d.next_eligible_at)}`));
        const wake=report.relay.last_wake?.[slot];if(wake)card.append(element('p',`${tr('lastWake')}: ${wake.outcome} / ${wake.reason||'—'} · ${time(wake.at)}`));
        if(d.head_id){const inspect=element('button',tr('inspect'));inspect.type='button';inspect.addEventListener('click',()=>inspectMessage(d.head_id));card.append(inspect);}
        return card;
      }));
    }catch(e){status(e.message,true);}finally{$('diagnose').disabled=false;}
  });
  function renderAttention(value){
    // Activity refreshes must not steal focus from an attention control or make a
    // polite live region reannounce an unchanged summary.
    const key=JSON.stringify([value.summary,value.identities,language()]);if(key===attentionKey)return;attentionKey=key;
    const summary=value.summary||{}, items=[];
    const pending=Object.values(summary.inboxes||{}).reduce((n,i)=>n+(i.queued||0)+(i.delivering||0)+(i.unknown||0),0);
    if(pending){const button=element('button',`${tr('pendingTitle')} (${pending})`);button.type='button';button.dataset.pendingOpen='';button.addEventListener('click',()=>{showInspector(true);$('pending-title').scrollIntoView({block:'nearest'});});items.push(button);}
    for(const [slot,inbox] of Object.entries(summary.inboxes||{})){
      if(inbox.unknown)items.push(element('span',`${handle(slot)}: ${inbox.unknown} ${tr('unknown')}`,'badge'));
      if(inbox.oldest_queued_at)items.push(element('span',`${handle(slot)} · ${tr('oldestQueued')} ${time(inbox.oldest_queued_at)}`,'muted'));
    }
    for(const [slot,wake] of Object.entries(summary.last_wake||{}))if(wake.outcome==='failed')items.push(element('span',`${handle(slot)} · ${tr('wakeFailed')}: ${wake.reason}`,'badge'));
    if(summary.last_user_message){const button=element('button',tr('userAttention'));button.type='button';button.addEventListener('click',()=>inspectMessage(summary.last_user_message));items.push(button);}
    $('attention').replaceChildren(...items);$('attention').hidden=items.length===0;
  }
  let deliveryKey = '';
  function renderDelivery(value) {
    const key = JSON.stringify([value.summary, value.identities, language()]);
    if (key === deliveryKey) return;
    deliveryKey = key;
    $('delivery-summary').replaceChildren(...['slot1', 'slot2'].map(slot => {
      const inbox = value.summary?.inboxes?.[slot];
      const card = element('section', undefined, 'delivery-slot');
      card.append(element('h3', handle(slot)));
      if (!inbox) { card.append(element('p', tr('never'), 'muted')); return card; }
      const counts = element('dl', undefined, 'binding-meta');
      for (const state of ['queued', 'delivering', 'unknown']) {
        counts.append(element('dt', tr(state)), element('dd', String(inbox[state] || 0)));
      }
      card.append(counts);
      if (inbox.oldest_queued_at) card.append(element('p', `${tr('oldestQueued')}: ${time(inbox.oldest_queued_at)}`, 'muted'));
      const wake = value.summary?.last_wake?.[slot];
      if (wake) card.append(element('p', `${tr('lastWake')}: ${wake.outcome} · ${time(wake.at)}`, 'muted'));
      return card;
    }));
  }
  function renderAudit(value){const key=JSON.stringify([value.relay.audit,language()]);if(key===auditKey)return;auditKey=key;
    const names={'native.binding.updated':'changed','native.publication':'publication','native.publication.gap':'gap','native.message.updated':'updated','native.failure':'failure'};
    $('audit').replaceChildren(...(value.relay.audit||[]).slice(-80).reverse().map(a=>{const li=element('li');li.append(element('strong',tr(names[a.kind]||a.kind)),element('time',time(a.at)));if(a.detail)li.append(element('p',a.detail));return li;}));
  }
  function render(value){snapshot=value;restoreOutbox();renderAttention(value);refreshPending().catch(e=>status(e.message,true));$('room-name').textContent=value.room.name;document.title=`${value.room.name} · PairRoom Native`;renderBindings(value);renderMessages(value);renderDelivery(value);renderAudit(value);}
  async function refresh(){if(refreshing){refreshAgain=true;return;}refreshing=true;try{do{refreshAgain=false;const value=await request('api/v1/snapshot?tail=1');if(!snapshot || value.relay.sequence>=snapshot.relay.sequence)render(value);}while(refreshAgain);}finally{refreshing=false;}}
  function lockComposer(){
    // The Room id scopes the recovery record, so publication stays locked until
    // the first snapshot identifies it; a click can never be silently dropped.
    // Drafting stays editable, matching the oversized-draft rule.
    for(const id of ['message-text','target','attachment','review-anchor'])$(id).disabled=sending||pendingSend!==null||outboxBroken;
    $('send').disabled=!outboxRoom||sending||outboxBroken;
    $('send').textContent=pendingSend?tr('retryOriginal'):tr('send');
    $('outbox-check').disabled=sending||outboxBroken;
    $('outbox-forget').disabled=sending;
  }
  function renderOutbox(){
    $('outbox').hidden=!pendingSend&&!outboxBroken;
    $('outbox-notice').textContent=tr(outboxBroken?'storageFailed':'savedDraft');
    lockComposer();
  }
  function restoreOutbox(){
    if(outboxRoom===snapshot.room.id)return;
    outboxRoom=snapshot.room.id;
    try{pendingSend=outbox.load(localStorage,outboxRoom);}catch(_){outboxBroken=true;}
    if(pendingSend){$('message-text').value=pendingSend.text;$('target').value=pendingSend.to;$('review-anchor').checked=Boolean(pendingSend.review);}
    renderOutbox();
    // Read-only reconciliation is safe; loading never posts a message.
    if(pendingSend)checkOriginal().catch(e=>status(failureText(e),true));
  }
  async function checkOriginal(){
    if(!pendingSend||sending)return;
    const original=pendingSend;
    const receipt=await request(`api/v1/sends/${encodeURIComponent(original.id)}`);
    if(pendingSend!==original)return;
    if(!receipt.found){status(tr('notFoundReceipt'));return;}
    if(!outbox.matches(original,receipt.message)){outboxBroken=true;renderOutbox();throw new Error(tr('receiptConflict'));}
    outbox.clear(localStorage,outboxRoom,original.id);
    pendingSend=null;$('message-text').value='';$('attachment').value='';$('review-anchor').checked=false;
    renderOutbox();status(tr('receiptRecovered'));await refresh();
  }
  $('outbox-check').addEventListener('click',()=>checkOriginal().catch(e=>status(failureText(e),true)));
  $('outbox-forget').addEventListener('click',async()=>{
    if(sending||!await confirmAction('forgetTitle','forgetBody'))return;
    try{outbox.forget(localStorage,outboxRoom);pendingSend=null;outboxBroken=false;$('message-text').value='';$('attachment').value='';renderOutbox();status(tr('forgotten'));}catch(_){status(tr('storageFailed'),true);}
  });
  $('composer').addEventListener('submit',async event=>{
    event.preventDefault();if(sending||outboxBroken||!outboxRoom)return;
    const text=$('message-text').value.trim();const file=$('attachment').files[0];
    if(!pendingSend&&!text&&!file){status(tr('inputRequired'),true);return;}
    if(!pendingSend && new TextEncoder().encode(text).byteLength>MAX_MESSAGE_BYTES){status(`${window.PairRoomI18n.t('errors.request_too_large')} (256 KiB UTF-8)`,true);return;}
    sending=true;lockComposer();
    try{
      if(!pendingSend){
        let attachmentIDs=[];
        let review;
        if($('review-anchor').checked)review=await request('api/v1/review');
        if(file){const body=new FormData();body.append('file',file);const a=await request('api/v1/attachments',{method:'POST',body});attachmentIDs=[a.id];}
        const draft={id:crypto.randomUUID(),text,to:$('target').value,attachment_ids:attachmentIDs};if(review)draft.review=review;
        // A storage failure prevents publication. The draft remains editable.
        outbox.save(localStorage,outboxRoom,draft);pendingSend=draft;
      }else{
        outbox.save(localStorage,outboxRoom,pendingSend); // Detect another tab changing the recovery record.
      }
      const accepted=await request('api/v1/messages',{method:'POST',body:JSON.stringify(pendingSend)});
      if(!outbox.matches(pendingSend,accepted))throw new Error(tr('receiptConflict'));
      outbox.clear(localStorage,outboxRoom,pendingSend.id);
      pendingSend=null;$('message-text').value='';$('attachment').value='';$('review-anchor').checked=false;status(tr('sent'));await refresh();
    }catch(e){status(pendingSend?`${tr('unknownSend')} ${failureText(e)}`:failureText(e),true);}finally{sending=false;renderOutbox();}
  });
  window.addEventListener('storage',event=>{
    if(event.key===`pairroom.native.outbox.v1.${outboxRoom}`&&!sending){if(pendingSend){checkOriginal().catch(e=>status(failureText(e),true));}else{outboxRoom='';restoreOutbox();}}
  });
  document.addEventListener('pairroom:lang',translations);
  async function start(){
    translations();lockComposer();try{
      const token=new URLSearchParams(location.hash.slice(1)).get('token');
      if(token){const session=await request('api/v1/session',{method:'POST',headers:{Authorization:`Bearer ${token}`}});csrf=session.csrf_token;history.replaceState(null,'',location.pathname+location.search);}
      else{const session=await request('api/v1/session');csrf=session.csrf_token;}
      await refresh();stream=new EventSource('api/v1/events');stream.addEventListener('native',()=>{refresh().catch(e=>status(e.message,true));});activityTimer=setInterval(()=>{if(!document.hidden)refresh().catch(()=>{});},15000);stream.onopen=()=>{$('connection').textContent=tr('connected');};stream.onerror=()=>{$('connection').textContent=tr('reconnecting');};
    }catch(e){status(e.message,true);}
  }
  window.addEventListener('pagehide',()=>{stream?.close();clearInterval(activityTimer);});window.addEventListener('pageshow',event=>{if(event.persisted)start();});start();
})();
