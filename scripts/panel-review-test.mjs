// PLAYWRIGHT_MODULE and CHROMIUM_PATH may point to an existing local install.
import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true, executablePath:process.env.CHROMIUM_PATH || undefined, args:['--no-sandbox']});
const page = await browser.newPage();
const html = fs.readFileSync(new URL('../web/index.html', import.meta.url), 'utf8');
const failures = [];
page.on('pageerror', e => failures.push(e.message));
let releaseB;
let holdB = false;
let rejectB = false;
const writes = [];
await page.route('http://panel.test/**', async route => {
  const url = new URL(route.request().url());
  const idx = url.searchParams.get('auth_index') || 'a';
  let data = {};
  if (url.pathname.endsWith('/index')) return route.fulfill({contentType:'text/html',body:html});
  if (url.pathname.endsWith('/credentials')) data = {credentials:['a','b'].map(auth_index=>({auth_index,name:auth_index+'.json',label:auth_index})),test_defaults:{model:'m',prompt:'hi',endpoint:'https://chatgpt.com/backend-api/codex/responses'}};
  if (url.pathname.endsWith('/rule')) {
    if (route.request().method() === 'PUT') writes.push(route.request().postDataJSON());
    if (idx === 'b' && holdB) await new Promise(r=>{releaseB=r;});
    if (idx === 'b' && rejectB) return route.fulfill({status:500,contentType:'application/json',body:'{"error":"unavailable"}'});
    data = {rule:{auth_index:idx,enabled:true,set:{'X-Owner':idx},remove:[],strip_foreign_turn_state:true}};
  }
  if (url.pathname.endsWith('/models')) data = {models:[{id:'m'}]};
  if (url.pathname.endsWith('/history')) data = {total:1,page:1,total_pages:1,items:[{id:idx,auth_index:idx,model:'m',started_at:'2026-09-19T00:00:00Z',outcome:'failed',status_code:500,error:'ERROR'.repeat(300)}]};
  // Deliberately return global data to exercise defensive client filtering.
  if (url.pathname.endsWith('/turn-states')) data = {turn_states:[{auth_index:'a',state:'state-a',model:'m',expired:false},{auth_index:'a',state:'old-a',model:'old',expired:true},{auth_index:'b',state:'state-b',model:'m',expired:false}]};
  await route.fulfill({contentType:'application/json',body:JSON.stringify(data)});
});
try {
  await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
  await page.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
  await page.waitForFunction(()=>document.querySelector('#summaryHealth').textContent==='已加载');
  await page.waitForFunction(()=>document.querySelector('#overviewStateCount').textContent==='1');
  assert.equal(await page.locator('#turnStateBody .state-card').count(),2);
  assert.equal(await page.locator('#overviewStateList').textContent().then(s=>s.includes('state-b')),false);
  assert.equal(await page.evaluate(()=>previewAfterHeaders({'x-codex-turn-state':'client'})['x-codex-turn-state']),'client');
  assert.equal(await page.locator('#overviewTestHeaders').textContent().then(s=>s.includes('OpenAI/Python')),false);
  holdB=true;
  await page.evaluate(()=>{selectCredential('b').catch(()=>{});});
  await page.waitForFunction(()=>document.querySelector('#saveRule').disabled);
  assert.equal(await page.locator('#setRows input').count(),0);
  assert.equal(await page.locator('#diffBox').textContent(),'');
  await page.evaluate(()=>document.querySelector('#saveRule').click());
  assert.equal(writes.length,0);
  await page.evaluate(()=>selectCredential('a'));
  while (!releaseB) await new Promise(r=>setTimeout(r,10));
  releaseB();
  await page.waitForTimeout(100);
  assert.equal(await page.locator('#setRows .hv').inputValue(),'a');
  holdB=false; rejectB=true;
  await page.evaluate(()=>selectCredential('b').catch(()=>{}));
  assert.equal(await page.locator('#saveRule').isDisabled(),true);
  assert.equal(await page.locator('#summaryHealth').textContent(),'未加载');
  assert.equal(await page.locator('#reloadRule').isEnabled(),true);
  rejectB=false;
  await page.evaluate(()=>loadRule());
  assert.equal(await page.locator('#saveRule').isEnabled(),true);
  assert.equal(await page.locator('#setRows .hv').inputValue(),'b');
  await page.evaluate(()=>selectCredential('a'));
  for (const width of [1440,1100,900,821,390]) {
    await page.setViewportSize({width,height:1000});
    for (const workspace of ['rules','history']) {
      await page.evaluate(v=>setWorkspace(v,false),workspace);
      const overflow=await page.evaluate(()=>document.documentElement.scrollWidth-innerWidth);
      assert.ok(overflow<=1,`${width}px ${workspace} overflow ${overflow}`);
    }
  }
  assert.deepEqual(failures,[]);
  console.log('panel review regressions: passed');
} finally { await browser.close(); }
