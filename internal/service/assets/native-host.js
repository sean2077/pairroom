(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const tr = key => window.PairRoomI18n.t(`room.native.${key}`);
  let csrf = '', snapshot = null, stream = null, pendingSend = null, refreshing = false, refreshAgain = false;
  const messageNodes = new Map();
  let bindingsKey = '', auditKey = '', activityTimer = null;
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
  function handle(slot) { return snapshot?.identities?.[slot]?.MentionHandle || (slot === 'user' ? tr('user') : slot); }
  function translations() {
    window.PairRoomI18n.apply(document);
    $('message-text').placeholder=tr('placeholder'); bindingsKey='';auditKey='';messageNodes.forEach(v=>{v.key='';});
    if (snapshot) render(snapshot);
  }
  function renderBindings(value) {
    const key=JSON.stringify([value.relay.bindings,value.room.agents,language()]);if(key===bindingsKey)return;bindingsKey=key;
    const nodes=['claude','codex'].map((slot,index)=>{
      const b=value.relay.bindings[slot]||{};const selection=value.room.agents[slot]||{};
      const card=element('article',undefined,'binding');card.dataset.slot=slot;
      const top=element('div',undefined,'binding-top');top.append(element('h3',`${index+1} · ${handle(slot)}`),element('span',tr(!b.active?'unbound':b.session_id?'bound':'pending'),'badge'));
      if(b.active){const park=element('button',tr(b.park_enabled?'parkOn':'parkOff'));park.type='button';park.dataset.park=slot;park.setAttribute('aria-pressed',String(Boolean(b.park_enabled)));park.addEventListener('click',async()=>{park.disabled=true;try{await request(`api/v1/participants/${slot}/park`,{method:'POST',body:JSON.stringify({enabled:!b.park_enabled})});status(tr('controls'));await refresh();}catch(e){status(e.message,true);}finally{park.disabled=false;}});top.append(park);}
      card.append(top);const meta=element('dl',undefined,'binding-meta');[[tr('binding'),b.bind_id||'—'],[tr('generation'),b.generation||'—'],[tr('session'),b.session_id||'—'],[tr('last'),time(b.last_activity)]].forEach(([k,v])=>meta.append(element('dt',k),element('dd',String(v))));card.append(meta);
      const instructions=element('details');instructions.append(element('summary',tr('commands')),element('p',tr('bindingHint'),'muted'));
      instructions.append(element('pre',`pairroom relay install --runtime ${selection.runtime}\npairroom relay bind --room ${value.room.id} --slot ${slot}\npairroom relay wait --room ${value.room.id} --slot ${slot}`));card.append(instructions);
      const config=element('details');config.append(element('summary',tr('metadata')),element('pre',JSON.stringify(selection,null,2)));card.append(config);return card;
    });$('bindings').replaceChildren(...nodes);
    for(const option of $('target').options) option.textContent=handle(option.value);
  }
  async function confirmRetry(){const dialog=$('confirm-dialog');dialog.returnValue='cancel';return new Promise(resolve=>{dialog.addEventListener('close',()=>resolve(dialog.returnValue==='confirm'),{once:true});dialog.showModal();});}
  function renderMessages(value){
    const messages=value.relay.messages||[];const active=new Set(messages.map(m=>m.id));
    for(const [id,v] of messageNodes){if(!active.has(id)){v.node.remove();messageNodes.delete(id);}}
    $('message-count').textContent=String(messages.length);$('messages').querySelector('.empty')?.remove();
    if(!messages.length){$('messages').append(element('div',tr('empty'),'empty'));return;}
    const nearEnd=$('messages').scrollHeight-$('messages').scrollTop-$('messages').clientHeight<70;
    for(const m of messages){
      const key=JSON.stringify([m,language()]);let entry=messageNodes.get(m.id);if(!entry){entry={node:element('article'),key:''};messageNodes.set(m.id,entry);$('messages').append(entry.node);}
      if(key===entry.key)continue;entry.key=key;const node=entry.node;node.className=`message state-${m.state}`;node.dataset.messageId=m.id;
      const head=element('div',undefined,'message-heading');head.append(element('strong',`${handle(m.from)} → ${handle(m.to)}`),element('span',tr(m.state),'badge'));
      const items=[head];if(m.quote)items.push(element('blockquote',`${m.quote.from_handle || ''}\n${m.quote.text || ''}`));items.push(element('p',m.text,'message-body'));
      for(const a of m.attachments||[]){const img=element('img');img.alt=a.name||tr('attach');img.loading='lazy';img.src=`api/v1/attachments/${encodeURIComponent(a.id)}`;items.push(img);}
      const stamp=element('time',time(m.created_at));stamp.dateTime=m.created_at;items.push(stamp);
      if(m.state==='queued'||m.state==='unknown'){
        const action=m.state==='queued'?'cancel':'retry';const button=element('button',tr(action),'message-action');button.type='button';button.dataset.action=action;
        button.addEventListener('click',async()=>{if(action==='retry' && !await confirmRetry())return;button.disabled=true;try{await request(`api/v1/messages/${encodeURIComponent(m.id)}/${action}`,{method:'POST',body:'{}'});await refresh();}catch(e){status(e.message,true);}finally{button.disabled=false;}});items.push(button);
      }
      node.replaceChildren(...items);
    }
    if(nearEnd)$('messages').scrollTop=$('messages').scrollHeight;
  }
  function renderAudit(value){const key=JSON.stringify([value.relay.audit,language()]);if(key===auditKey)return;auditKey=key;
    const names={'native.binding.updated':'changed','native.publication':'publication','native.publication.gap':'gap','native.message.updated':'updated','native.failure':'failure'};
    $('audit').replaceChildren(...(value.relay.audit||[]).slice(-80).reverse().map(a=>{const li=element('li');li.append(element('strong',tr(names[a.kind]||a.kind)),element('time',time(a.at)));if(a.detail)li.append(element('p',a.detail));return li;}));
  }
  function render(value){snapshot=value;$('room-name').textContent=value.room.name;document.title=`${value.room.name} · PairRoom Native`;renderBindings(value);renderMessages(value);renderAudit(value);}
  async function refresh(){if(refreshing){refreshAgain=true;return;}refreshing=true;try{do{refreshAgain=false;const value=await request('api/v1/snapshot');if(!snapshot || value.relay.sequence>=snapshot.relay.sequence)render(value);}while(refreshAgain);}finally{refreshing=false;}}
  $('composer').addEventListener('submit',async event=>{
    event.preventDefault();const text=$('message-text').value.trim();const file=$('attachment').files[0];if(!text&&!file){status(tr('inputRequired'),true);return;}
    const to=$('target').value;
    if(pendingSend && (pendingSend.text!==text||pendingSend.to!==to)){status(tr('unknownSend'),true);return;}
    $('send').disabled=true;
    try{
      if(!pendingSend){let attachmentIDs=[];if(file){const body=new FormData();body.append('file',file);const a=await request('api/v1/attachments',{method:'POST',body});attachmentIDs=[a.id];}pendingSend={id:crypto.randomUUID(),text,to,attachment_ids:attachmentIDs};}
      await request('api/v1/messages',{method:'POST',body:JSON.stringify(pendingSend)});pendingSend=null;$('message-text').value='';$('attachment').value='';status(tr('sent'));await refresh();
    }catch(e){status(e.message,true);}finally{$('send').disabled=false;}
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
