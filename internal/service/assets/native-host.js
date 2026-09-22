(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const tr = key => window.PairRoomI18n.t(`room.native.${key}`);
  let csrf = '', snapshot = null, stream = null, pendingSend = null, refreshing = false, refreshAgain = false, sending = false;
  const messageNodes = new Map();
  let bindingsKey = '', auditKey = '', activityTimer = null;
  let outboxRoom = '', outboxBroken = false, pendingCursor = '', pendingNext = '', pendingKey = '', pendingRequest = 0;
  let historyNext = '', historyFilter = '', historyRequest = 0;
  const outbox = window.PairRoomNativeOutbox;
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
  function translations() {
    window.PairRoomI18n.apply(document);
    $('message-text').placeholder=tr('placeholder'); bindingsKey='';auditKey='';pendingKey='';messageNodes.forEach(v=>{v.key='';});
    if (snapshot) { render(snapshot); renderOutbox(); }
  }
  function renderBindings(value) {
    const key=JSON.stringify([value.relay.bindings,value.room.agents,language()]);if(key===bindingsKey)return;bindingsKey=key;
    const nodes=['slot1','slot2'].map((slot,index)=>{
      const b=value.relay.bindings[slot]||{};const selection=value.room.agents[slot]||{};
      const card=element('article',undefined,'binding');card.dataset.slot=slot;
      const top=element('div',undefined,'binding-top');top.append(element('h3',`${index+1} · ${handle(slot)}`),element('span',tr(!b.active?'unbound':b.session_id?'bound':'pending'),'badge'));
      if(b.active){const park=element('button',tr(b.park_enabled?'parkOn':'parkOff'));park.type='button';park.dataset.park=slot;park.setAttribute('aria-pressed',String(Boolean(b.park_enabled)));park.addEventListener('click',async()=>{park.disabled=true;try{await request(`api/v1/participants/${slot}/park`,{method:'POST',body:JSON.stringify({enabled:!b.park_enabled})});status(tr('controls'));await refresh();}catch(e){status(e.message,true);}finally{park.disabled=false;}});top.append(park);}
      card.append(top);const meta=element('dl',undefined,'binding-meta');[[tr('binding'),b.bind_id||'—'],[tr('generation'),b.generation||'—'],[tr('session'),b.session_id||'—'],[tr('last'),time(b.last_activity)]].forEach(([k,v])=>meta.append(element('dt',k),element('dd',String(v))));card.append(meta);
      const instructions=element('details');instructions.append(element('summary',tr('commands')),element('p',tr('bindingHint'),'muted'));
      instructions.append(element('pre',`pairroom relay install --runtime ${selection.runtime}\npairroom relay bind --room ${value.room.id} --slot ${index+1}\npairroom relay wait --room ${value.room.id} --slot ${index+1}`));card.append(instructions);
      const config=element('details');config.append(element('summary',tr('metadata')),element('pre',JSON.stringify(selection,null,2)));card.append(config);return card;
    });$('bindings').replaceChildren(...nodes);
    for(const option of $('target').options) option.textContent=handle(option.value);
  }
  async function confirmAction(title,body){$('confirm-retry').textContent=title==='retryTitle'?tr('retry'):tr('forgetDraft');$('confirm-title').textContent=tr(title);$('confirm-body').textContent=tr(body);const dialog=$('confirm-dialog');dialog.returnValue='cancel';return new Promise(resolve=>{dialog.addEventListener('close',()=>resolve(dialog.returnValue==='confirm'),{once:true});dialog.showModal();});}
  const confirmRetry=()=>confirmAction('retryTitle','retryBody');
  const MAX_RENDERED_MESSAGES=300;
  const MAX_MESSAGE_BYTES=256*1024;
  function renderMessages(value){
    const all=value.relay.messages||[];
    const total=value.relay.total_messages ?? all.length;
    // Both the HTTP projection and DOM are bounded; totals still describe the
    // complete retained history, available through the explicit export API.
    const messages=all.length>MAX_RENDERED_MESSAGES?all.slice(-MAX_RENDERED_MESSAGES):all;
    const active=new Set(messages.map(m=>m.id));
    for(const [id,v] of messageNodes){if(!active.has(id)){v.node.remove();messageNodes.delete(id);}}
    $('message-count').textContent=String(total);$('messages').querySelector('.empty')?.remove();$('messages').querySelectorAll('.truncated-note').forEach(n=>n.remove());
    if(!all.length){$('messages').append(element('div',tr('empty'),'empty'));return;}
    if(total>messages.length){$('messages').prepend(element('div',`${tr('showingLatest')} ${messages.length} / ${total}`,'muted truncated-note'));}
    const nearEnd=$('messages').scrollHeight-$('messages').scrollTop-$('messages').clientHeight<70;
    for(const m of messages){
      const key=JSON.stringify([m,language()]);let entry=messageNodes.get(m.id);if(!entry){entry={node:element('article'),key:''};messageNodes.set(m.id,entry);$('messages').append(entry.node);}
      if(key===entry.key)continue;entry.key=key;const node=entry.node;node.className=`message state-${m.state}`;node.dataset.messageId=m.id;
      fillMessage(node,m,'chat');
    }
    if(nearEnd)$('messages').scrollTop=$('messages').scrollHeight;
  }
  function fillMessage(node,m,view){
    const head=element('div',undefined,'message-heading');head.append(element('strong',`${handle(m.from)} → ${handle(m.to)}`),element('span',tr(m.state),'badge'));
    const items=[head];
    if(m.quote)items.push(element('blockquote',`${m.quote.from_handle || ''}\n${m.quote.text || ''}`));
    const body=element('div',undefined,'message-body');
    const content=()=>window.PairRoomRichText.render(body,m.text,{createImage:(_ref,alt)=>element('span',alt||tr('externalImage'),'muted'),onCopyError:()=>status(tr('copyFailed'),true)});
    if(m.text.length>3000||view==='pending'){
      const details=element('details');details.append(element('summary',m.text.slice(0,120)||tr('evidence')));let loaded=false;
      details.addEventListener('toggle',()=>{if(details.open&&!loaded){loaded=true;content();}});details.append(body);items.push(details);
    }else{content();items.push(body);}
    if(m.review){
      const evidence=element('details',undefined,'review-evidence');evidence.append(element('summary',`${tr('reviewVersion')} · ${m.review.head.slice(0,12)}`),element('pre',JSON.stringify(m.review,null,2)));
      const check=element('button',tr('checkReview'));check.type='button';check.dataset.review=m.id;
      const result=element('span',tr('reviewUnverified'),'muted');check.addEventListener('click',async()=>{check.disabled=true;try{const value=await request(`api/v1/review?id=${encodeURIComponent(m.id)}`);result.textContent=tr(`review_${value.status}`);}catch(e){status(e.message,true);}finally{check.disabled=false;}});
      evidence.append(check,result);items.push(evidence);
    }
    for(const a of m.attachments||[]){const img=element('img');img.alt=a.name||tr('attach');img.loading='lazy';img.src=`api/v1/attachments/${encodeURIComponent(a.id)}`;items.push(img);}
    const stamp=element('time',time(m.created_at));stamp.dateTime=m.created_at;items.push(stamp);
    const inspect=element('button',tr('inspect'),'message-action');inspect.type='button';inspect.addEventListener('click',()=>inspectMessage(m.id));items.push(inspect);
    if(m.state==='queued'||m.state==='unknown'){
      const action=m.state==='queued'?'cancel':'retry';const button=element('button',tr(action),'message-action');button.type='button';button.dataset.action=action;
      button.addEventListener('click',async()=>{if(action==='retry'&&!await confirmRetry())return;button.disabled=true;try{await request(`api/v1/messages/${encodeURIComponent(m.id)}/${action}`,{method:'POST',body:'{}'});pendingKey='';await refresh();}catch(e){status(e.message,true);}finally{button.disabled=false;}});items.push(button);
    }
    node.replaceChildren(...items);
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
  function inspectMessage(id){$('history-panel').open=true;$('history-id').value=id;$('history-since').value='';historyFilter=`&id=${encodeURIComponent(id)}`;loadHistory().then(()=>$('history-panel').scrollIntoView({block:'nearest'})).catch(e=>status(e.message,true));}
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
    const summary=value.summary||{}, items=[];
    for(const [slot,inbox] of Object.entries(summary.inboxes||{})){
      if(inbox.unknown)items.push(element('span',`${handle(slot)}: ${inbox.unknown} ${tr('unknown')}`,'badge'));
      if(inbox.oldest_queued_at)items.push(element('span',`${handle(slot)} · ${tr('oldestQueued')} ${time(inbox.oldest_queued_at)}`,'muted'));
    }
    for(const [slot,wake] of Object.entries(summary.last_wake||{}))if(wake.outcome==='failed')items.push(element('span',`${handle(slot)} · ${tr('wakeFailed')}: ${wake.reason}`,'badge'));
    if(summary.last_user_message){const button=element('button',tr('userAttention'));button.type='button';button.addEventListener('click',()=>inspectMessage(summary.last_user_message));items.push(button);}
    $('attention').replaceChildren(...items);$('attention').hidden=items.length===0;
  }
  function renderAudit(value){const key=JSON.stringify([value.relay.audit,language()]);if(key===auditKey)return;auditKey=key;
    const names={'native.binding.updated':'changed','native.publication':'publication','native.publication.gap':'gap','native.message.updated':'updated','native.failure':'failure'};
    $('audit').replaceChildren(...(value.relay.audit||[]).slice(-80).reverse().map(a=>{const li=element('li');li.append(element('strong',tr(names[a.kind]||a.kind)),element('time',time(a.at)));if(a.detail)li.append(element('p',a.detail));return li;}));
  }
  function render(value){snapshot=value;restoreOutbox();renderAttention(value);refreshPending().catch(e=>status(e.message,true));$('room-name').textContent=value.room.name;document.title=`${value.room.name} · PairRoom Native`;renderBindings(value);renderMessages(value);renderAudit(value);}
  async function refresh(){if(refreshing){refreshAgain=true;return;}refreshing=true;try{do{refreshAgain=false;const value=await request('api/v1/snapshot?tail=1');if(!snapshot || value.relay.sequence>=snapshot.relay.sequence)render(value);}while(refreshAgain);}finally{refreshing=false;}}
  function lockComposer(){
    for(const id of ['message-text','target','attachment','review-anchor'])$(id).disabled=sending||pendingSend!==null||outboxBroken;
    $('send').disabled=sending||outboxBroken;
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
    if(pendingSend)checkOriginal().catch(e=>status(e.message,true));
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
  $('outbox-check').addEventListener('click',()=>checkOriginal().catch(e=>status(e.message,true)));
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
    }catch(e){status(pendingSend?`${tr('unknownSend')} ${e.message}`:e.message,true);}finally{sending=false;renderOutbox();}
  });
  window.addEventListener('storage',event=>{
    if(event.key===`pairroom.native.outbox.v1.${outboxRoom}`&&!sending){if(pendingSend){checkOriginal().catch(e=>status(e.message,true));}else{outboxRoom='';restoreOutbox();}}
  });
  document.addEventListener('pairroom:lang',translations);
  async function start(){
    translations();try{
      const token=new URLSearchParams(location.hash.slice(1)).get('token');
      if(token){const session=await request('api/v1/session',{method:'POST',headers:{Authorization:`Bearer ${token}`}});csrf=session.csrf_token;history.replaceState(null,'',location.pathname+location.search);}
      else{const session=await request('api/v1/session');csrf=session.csrf_token;}
      await refresh();stream=new EventSource('api/v1/events');stream.addEventListener('native',()=>{refresh().catch(e=>status(e.message,true));});activityTimer=setInterval(()=>{if(!document.hidden)refresh().catch(()=>{});},15000);stream.onopen=()=>{$('connection').textContent=tr('connected');};stream.onerror=()=>{$('connection').textContent=tr('reconnecting');};
    }catch(e){status(e.message,true);}
  }
  window.addEventListener('pagehide',()=>{stream?.close();clearInterval(activityTimer);});window.addEventListener('pageshow',event=>{if(event.persisted)start();});start();
})();
