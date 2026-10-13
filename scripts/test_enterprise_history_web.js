'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { fixture, signIn, open, history, session, headers, deferred, flush } = require('./enterprise_web_test_helpers');
const message = n => ({ id:'message-'+n, from:'user', to:'slot1', state:'cancelled', text:'complete evidence '+n });
async function tail(f, id='room-a', total=401, start=101) { f.element('room-id').value=id;const opening=f.element('room-form').fire('submit');await flush();f.reply('/rooms/'+id,200,{...history(id),sequence:800,total_messages:total,messages:Array.from({length:total-start},(_,i)=>message(start+i))});await opening;return f; }
const ids=f=>f.element('messages').children.map(x=>x.children[1].textContent);

test('bounded tail reports exact total and older pages prepend in publication order with original cursor and no mutation journal',async()=>{
 const f=await tail(await signIn());assert.match(f.element('room-sequence').textContent,/401 messages · 300 loaded/);assert.equal(f.element('history-older').hidden,false);
 const reading=f.element('history-older').fire('click');await flush();const r=f.take('/rooms/room-a/history');assert.deepEqual(JSON.parse(r.options.body),{cursor:'history:101',limit:100});assert.equal(r.options.headers['X-PairRoom-CSRF'],'private-csrf-session-a');
 f.complete(r,200,{messages:Array.from({length:100},(_,i)=>message(100-i)),has_more:true,next_cursor:'history:1',total:401,sequence:800});await reading;assert.match(f.element('room-sequence').textContent,/401 messages · 400 loaded/);assert.equal(ids(f)[0],'complete evidence 1');assert.equal(ids(f).at(-1),'complete evidence 400');
 const last=f.element('history-older').fire('click');await flush();assert.deepEqual(JSON.parse(f.take('/rooms/room-a/history').options.body),{cursor:'history:1',limit:100});f.reply('/rooms/room-a/history',200,{messages:[message(0)],has_more:false,total:401,sequence:800});await last;assert.equal(f.element('history-older').hidden,true);assert.equal(ids(f)[0],'complete evidence 0');assert.match(f.element('history-status').textContent,/All retained/);assert.equal(f.storage.length,0);assert.equal(f.requests.filter(x=>x.options.method).every(x=>x.url.endsWith('/history')),true);
});

test('history is explicit, rejects duplicate clicks while loading and preserves cursor after unavailable or malformed pages',async()=>{
 const f=await tail(await signIn());assert.equal(f.requests.some(x=>x.options.method),false);const reading=f.element('history-older').fire('click');f.element('history-older').fire('click');await flush();assert.equal(f.requests.filter(x=>x.url.endsWith('/history')).length,1);f.reply('/rooms/room-a/history',503,{});await reading;assert.match(f.element('history-status').textContent,/original cursor/);assert.equal(f.element('history-older').disabled,false);assert.equal(f.element('messages').children.length,300);
 const retry=f.element('history-older').fire('click');await flush();assert.equal(JSON.parse(f.take('/rooms/room-a/history').options.body).cursor,'history:101');f.reply('/rooms/room-a/history',200,{messages:[message(100)],has_more:true,next_cursor:'history:101',total:401,sequence:800});await retry;assert.equal(f.element('messages').children.length,300);assert.match(f.element('history-status').textContent,/unavailable/);
});

test('publication append does not change the original older cursor and exact higher totals ask for explicit refresh',async()=>{
 const f=await tail(await signIn(), 'room-a',301,1);const reading=f.element('history-older').fire('click');await flush();assert.equal(JSON.parse(f.take('/rooms/room-a/history').options.body).cursor,'history:1');f.reply('/rooms/room-a/history',200,{messages:[message(0)],has_more:false,total:302,sequence:801});await reading;assert.match(f.element('room-sequence').textContent,/302 messages · 301 loaded/);assert.match(f.element('history-status').textContent,/Refresh Room/);assert.equal(f.requests.filter(r=>r.url==='/enterprise/api/v1/rooms/room-a').length,1);
});

test('latest Room wins when an old history response ignores cancellation and late history never changes another Room controls',async()=>{
 const f=await tail(await signIn());const reading=f.element('history-older').fire('click');await flush();const old=f.take('/rooms/room-a/history');await open(f,'room-b','current Room B');f.complete(old,200,{messages:[message(0)],has_more:false,total:401,sequence:800});await reading;assert.equal(f.element('room-title').textContent,'room-b');assert.deepEqual(ids(f),['current Room B']);assert.equal(f.element('history-older').hidden,true);assert.equal(f.element('history-older').disabled,false);
});

test('account and same-User session refresh discard pending history JSON before it can restore private evidence',async()=>{
 for(const identity of [session('user-b','session-b'),session('user-a','new-session')]){const f=await tail(await signIn());const reading=f.element('history-older').fire('click');await flush();const old=f.take('/rooms/room-a/history'),json=deferred();old.resolved=true;old.resolve({status:200,ok:true,headers:headers(session()),json:()=>json.promise});await flush();const refresh=f.element('refresh').fire('click');f.reply('/session',200,identity,identity);await refresh;json.resolve({messages:[message(0)],has_more:false,total:401,sequence:800});await reading;assert.equal(f.element('messages').children.length,0);assert.equal(f.element('room').hidden,true);assert.equal(f.element('history-status').textContent,'');}
});

test('live Room authorization refusal removes the existing tail and older controls without silently claiming all history is loaded',async()=>{
 const f=await tail(await signIn());const reading=f.element('history-older').fire('click');await flush();f.reply('/rooms/room-a/history',403,{outcome:'refused',class:'authorization_failed'});await reading;assert.equal(f.element('room').hidden,true);assert.equal(f.element('messages').children.length,0);assert.equal(f.element('history-older').hidden,true);assert.match(f.element('status').textContent,/Room access changed/);
});

test('older overlap is deduplicated while text, author and attachment evidence remain full plain text',async()=>{
 const f=await tail(await signIn(), 'room-a',301,1);const reading=f.element('history-older').fire('click');await flush();const text='full '.repeat(10000);f.reply('/rooms/room-a/history',200,{messages:[message(1),{...message(0),text,author:'enterprise:user-original',attachments:[{id:'att-0123456789abcdef01234567',name:'<image>.png',size:68}]}],has_more:false,total:301,sequence:800});await reading;assert.equal(f.element('messages').children.length,301);assert.equal(ids(f)[0],text);assert.match(f.element('messages').children[0].text(),/User user-original/);assert.equal(f.element('messages').children[0].children.flatMap(x=>x.children).find(x=>x.href).textContent,'<image>.png · 68 bytes');
});

test('an incomplete final page cannot silently mark omitted retained messages as fully loaded',async()=>{
 const f=await tail(await signIn());const reading=f.element('history-older').fire('click');await flush();f.reply('/rooms/room-a/history',200,{messages:[message(100)],has_more:false,total:401,sequence:800});await reading;assert.equal(f.element('messages').children.length,300);assert.equal(f.element('history-older').hidden,false);assert.match(f.element('history-status').textContent,/unavailable/);assert.equal(f.element('history-status').textContent.includes('All retained'),false);
});
