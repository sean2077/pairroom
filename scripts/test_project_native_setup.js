'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function element(tag, props = {}, ...children) {
  return {tag, ...props, children: children.filter(Boolean),
    replaceChildren(...values) { this.children = values.filter(Boolean); }};
}
function find(root, predicate) {
  if (predicate(root)) return root;
  for (const child of root.children || []) { const match = find(child, predicate); if (match) return match; }
}
const flush = async () => { for (let i=0;i<12;i++) await Promise.resolve(); };
function fixture() {
  const sandbox = {window: {}, queueMicrotask};
  vm.runInNewContext(fs.readFileSync('internal/service/assets/project-native-setup.js','utf8'), sandbox);
  const calls = [], dialogs = [];
  let handler, active = true;
  const ui = sandbox.window.PairRoomProjectSetup.create({
    t: (key, values) => key + (values ? JSON.stringify(values) : ''), node: element,
    actionButton: (textContent,onClick,className,disabled=false) => element('button',{textContent,onClick,className,disabled}),
    api: async (path, options={}) => { calls.push({path,...options}); return handler(path,options); },
    confirm: dialog => dialogs.push(dialog), isCurrent: () => () => active,
  });
  return {ui,calls,dialogs,setHandler(fn){handler=fn;},setActive(value){active=value;}};
}
const project = {id:'project one',root:'/projects/<unsafe>&repo',available:true};
function status(state='missing', confirmation=false) {
  return {runtimes:['claude','codex','grok','gemini'].map(runtime=>({runtime,
    skill:{component:'skill',path:`.${runtime}/skills/pairroom-relay/SKILL.md`,state,revision:`${runtime}-${state}-v1`,can_install:state!=='current',needs_confirmation:confirmation},
    hook:{component:'hook',path:`.${runtime}/hooks.json`,state:'missing',revision:`${runtime}-hook-v1`,can_install:true,needs_confirmation:false},
  }))};
}
function component(root, name) { return find(root, item=>item['data-setup-component']===name); }
function button(root) { return find(root,item=>item.tag==='button'); }

(async()=>{
  const c=fixture();c.setHandler(()=>status());
  let host=c.ui.mount(project);await flush();
  assert.equal(c.calls.length,1);assert.equal(c.calls[0].method,undefined,'render is read-only');
  assert.equal(c.calls[0].path,'/api/v1/projects/project%20one/native-setup');
  assert.equal(find(component(host,'skill'),item=>item.tag==='code').textContent,'.claude/skills/pairroom-relay/SKILL.md','manifest-only responses remain readable');
  const install=button(component(host,'skill'));
  c.setHandler(()=>status('current'));
  await install.onClick();
  assert.equal(c.calls.length,2);
  assert.deepEqual(JSON.parse(c.calls[1].body),{runtime:'claude',component:'skill',revision:'claude-missing-v1',confirm_replace:false});
  assert.equal(button(component(host,'skill')).disabled,true,'configured item is not rewritten');

  const d=fixture();d.setHandler(()=>status('conflict',true));
  host=d.ui.mount(project);await flush();button(component(host,'skill')).onClick();
  assert.equal(d.dialogs.length,1);assert.equal(d.calls.length,1,'confirmation is required before a POST');
  assert.ok(d.dialogs[0].message.includes(project.root));assert.ok(d.dialogs[0].message.includes('pairroom-relay/SKILL.md'));
  assert.ok(d.dialogs[0].acknowledgement);
  // Closing the dialog without executing its action has no write side effect.
  await flush();assert.equal(d.calls.length,1);
  d.setHandler(()=>status('current'));await d.dialogs[0].action();
  assert.equal(JSON.parse(d.calls[1].body).confirm_replace,true);

  const bundleFiles=['.claude/skills/pairroom-relay/SKILL.md',
    '.claude/skills/pairroom-relay/references/enterprise.md',
    '.claude/skills/pairroom-relay/references/lan.md'];
  const bundleStatus=(state,confirmation)=>{
    const result=status(state,confirmation);result.runtimes[0].skill.files=[...bundleFiles];return result;
  };
  const bundle=fixture();bundle.setHandler(()=>bundleStatus('missing',false));
  host=bundle.ui.mount(project);await flush();
  for(const path of bundleFiles){
    assert.ok(find(component(host,'skill'),item=>item.tag==='code'&&item.textContent===path),'new installation displays every bundled file');
  }
  assert.ok(find(component(host,'skill'),item=>item.textContent==='workspace.nativeSetup.skillContents'));
  assert.equal(bundle.calls.length,1,'showing the bundle does not install it');
  bundle.setHandler(()=>bundleStatus('update_required',true));await button(host).onClick();
  button(component(host,'skill')).onClick();
  const review=bundle.dialogs[0];
  assert.ok(review);
  assert.equal(JSON.parse(review.message.slice('workspace.nativeSetup.confirmBody'.length)).paths,bundleFiles.join('\n'),'confirmation names every managed file');
  assert.equal(bundle.calls.filter(call=>call.method==='POST').length,0,'bundle replacement waits for confirmation');
  bundle.setHandler(()=>bundleStatus('current',false));await review.action();
  assert.equal(bundle.calls.filter(call=>call.method==='POST').length,1);
  assert.deepEqual(JSON.parse(bundle.calls[2].body),{runtime:'claude',component:'skill',revision:'claude-update_required-v1',confirm_replace:true});

  const e=fixture();e.setHandler(()=>status('conflict',true));
  host=e.ui.mount(project);await flush();button(component(host,'skill')).onClick();
  e.ui.mount({...project,id:'other',root:'/other'});await flush();
  await assert.rejects(e.dialogs[0].action(),/workspace.nativeSetup.changed/);
  assert.equal(e.calls.filter(call=>call.method==='POST').length,0,'old confirmation cannot target new project');

  const f=fixture();let resolve;
  f.setHandler(()=>new Promise(yes=>{resolve=yes;}));
  host=f.ui.mount(project);await flush();assert.equal(button(host).disabled,true);
  f.setActive(false);resolve(status());await flush();
  assert.equal(component(host,'skill'),undefined,'stale session response ignored');

  const g=fixture();g.setHandler(()=>status());
  host=g.ui.mount(project);await flush();
  let finish;g.setHandler(()=>new Promise(yes=>{finish=yes;}));
  const first=button(component(host,'skill')).onClick();
  const pendingButton=button(component(host,'hook'));assert.equal(pendingButton.disabled,true);
  pendingButton.onClick();assert.equal(g.calls.length,2,'duplicate click while pending ignored');
  finish(status('current'));await first;

  const h=fixture();h.setHandler(()=>status());host=h.ui.mount(project);await flush();
  h.setHandler(()=>{throw new Error('unknown response');});await button(component(host,'skill')).onClick();
  assert.equal(h.calls.length,2,'failed write is never retried automatically');
  assert.equal(component(host,'skill'),undefined,'unknown outcome requires a fresh check');
  assert.ok(find(host,node=>String(node.textContent).includes('workspace.nativeSetup.checkBeforeRetry')));

  const unavailable=fixture();unavailable.setHandler(()=>{throw new Error('must not call');});
  host=unavailable.ui.mount({...project,available:false});await flush();
  assert.equal(unavailable.calls.length,0);assert.equal(button(host).disabled,true);

  const locale={window:{}};vm.runInNewContext(fs.readFileSync('internal/webui/assets/catalogs.js','utf8'),locale);
  const en=locale.window.PairRoomLocales.en.translation, zh=locale.window.PairRoomLocales['zh-CN'].translation;
  const keys=Object.keys(en).filter(key=>key.startsWith('workspace.nativeSetup.')).sort();
  assert.deepEqual(keys,Object.keys(zh).filter(key=>key.startsWith('workspace.nativeSetup.')).sort());
  for(const key of keys){assert.ok(en[key]);assert.ok(zh[key]);}
  const html=fs.readFileSync('internal/service/assets/index.html','utf8');
  assert.ok(html.indexOf('/project-native-setup.js')<html.indexOf('/management.js'));
  console.log('project native setup: read-only loading, bundle paths, install, confirmation, stale responses, errors and locale coverage OK');
})().catch(error=>{console.error(error);process.exitCode=1;});
