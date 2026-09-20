import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import('playwright');
const b = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--no-sandbox']});
const p = await b.newPage({viewport:{width:1280,height:800}});
const errs=[]; p.on('pageerror',e=>errs.push(e.message));
await p.route('http://panel.test/**', async route => {
  const path = new URL(route.request().url()).pathname;
  if (path.endsWith('/index')) return route.fulfill({contentType:'text/html',body:fs.readFileSync(new URL('../web/index.html',import.meta.url),'utf8')});
  let data={};
  if (path.endsWith('/credentials')) data={plugin_version:'0',credentials:[{auth_index:'a',name:'a.json',plan_type:'team'}]};
  if (path.endsWith('/rule')) data={rule:{auth_index:'a',enabled:true,set:{},remove:[]}};
  if (path.endsWith('/history')) data={items:[],total:0};
  if (path.endsWith('/turn-states')) data={turn_states:[]};
  await route.fulfill({contentType:'application/json',body:JSON.stringify(data)});
});
await p.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
await p.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
await p.waitForFunction(()=>!!document.querySelector('.skip-link'));
await p.keyboard.press('Tab');
const first = await p.evaluate(()=>document.activeElement?.className||'');
assert.ok(first.includes('skip-link'),'skip link is the first tab stop, got: '+first);
// and it must actually move focus to the main region
await p.keyboard.press('Enter');
const target = await p.evaluate(()=>document.activeElement?.id||document.activeElement?.tagName);
const r = await p.evaluate(()=>({
  headings:[...document.querySelectorAll('h1,h2,h3,h4')].map(h=>h.tagName),
  themes:document.querySelectorAll('meta[name="theme-color"]').length,
  touch:getComputedStyle(document.querySelector('button')).touchAction,
  tap:getComputedStyle(document.documentElement).webkitTapHighlightColor,
  iconBtnNoLabel:[...document.querySelectorAll('button')].filter(x=>!x.textContent.trim()&&!x.getAttribute('aria-label')).length,
}));
assert.ok(!r.headings.includes('H3'),'headings stop at h2: '+r.headings.join(','));
assert.equal(r.headings[0],'H1');
assert.equal(r.themes,2); assert.equal(r.touch,'manipulation'); assert.equal(r.tap,'rgba(0, 0, 0, 0)');
assert.equal(r.iconBtnNoLabel,0);
// The favicon is an inline data URI built from web/icon.svg; a malformed one
// closes its own href and spills the rest of the SVG into the markup.
const fav = await p.evaluate(async ()=>{
  const link=document.querySelector('link[rel="icon"]');
  if (!link) return {missing:true};
  const img=new Image();
  const loaded=await new Promise(res=>{img.onload=()=>res(true);img.onerror=()=>res(false);img.src=link.href;});
  return {loaded,w:img.naturalWidth,h:img.naturalHeight,
    stray:document.head.innerHTML.includes('-->')||document.body.innerText.includes('-->')};
});
assert.ok(fav.loaded,'favicon decodes to an image');
assert.equal(fav.w,32); assert.equal(fav.h,32);
assert.equal(fav.stray,false,'no markup leaked out of the favicon href');
assert.deepEqual(errs,[]);
console.log('a11y passed; skip →',target,'; headings',[...new Set(r.headings)].join(','));
await b.close();
