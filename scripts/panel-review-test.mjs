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
    data = {rule:{auth_index:idx,enabled:true,set:{'X-Owner':idx},remove:[],inject_turn_state:true}};
  }
  if (url.pathname.endsWith('/models')) data = {models:[{id:'m'}]};
  if (url.pathname.endsWith('/history')) data = {total:1,page:1,total_pages:1,items:[{id:idx,auth_index:idx,
    model:'gpt-6-astra-preview-long',requested_model:'gpt-6-astra-preview-long',upstream_model:'gpt-6-astra-preview-long',
    request_effort:'xhigh',upstream_effort:'xhigh',model_mismatch:false,
    started_at:'2026-09-19T00:00:00Z',outcome:'failed',status_code:500,error:'ERROR'.repeat(300)}]};
  // Deliberately return global data to exercise defensive client filtering.
  if (url.pathname.endsWith('/turn-states')) data = {turn_states:[{auth_index:'a',state:'state-a',model:'m',expired:false},{auth_index:'a',state:'old-a',model:'old',expired:true},{auth_index:'b',state:'state-b',model:'m',expired:false}]};
  await route.fulfill({contentType:'application/json',body:JSON.stringify(data)});
});
try {
  await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
  await page.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
  await page.waitForFunction(()=>document.querySelector('#summaryHealth').textContent==='已加载');
  // The endpoint answers with every credential's pool; the panel must show only
  // the selected one's rows, which is the filtering this guards.
  await page.evaluate(()=>setWorkspace('pool',false));
  await page.waitForFunction(()=>document.querySelectorAll('#turnStateBody .pool-row').length===2);
  assert.equal((await page.locator('#turnStateBody').textContent()).includes('state-b'),false);
  assert.equal(await page.evaluate(()=>previewAfterHeaders({'x-codex-turn-state':'client'})['x-codex-turn-state']),'client');
  await page.evaluate(()=>setWorkspace('rules',false));
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
    // Neither the model name nor its effort may be covered at any width. The
    // column is measured from its content and the table's own minimum follows
    // it, so a viewport too narrow for both scrolls sideways instead of
    // clipping.
    await page.evaluate(()=>setWorkspace('history',false));
    await page.waitForTimeout(120);
    const cut = await page.evaluate(() => [...document.querySelectorAll('.history-model .mname, .history-model .effort')]
      .filter((el) => {
        const box = el.getBoundingClientRect();
        const cell = el.closest('td').getBoundingClientRect();
        return box.width === 0 || box.right > cell.right + 0.5 || el.scrollWidth > el.clientWidth + 1;
      })
      .map((el) => `${el.className}:${el.textContent}`));
    assert.deepEqual(cut, [], `${width}px: covered text ${JSON.stringify(cut)}`);
    // Below the width the table needs, the scroller is what gives way.
    const scrolls = await page.evaluate(() => {
      const sc = document.querySelector('#historyPanel .table-scroll');
      return sc ? sc.scrollWidth - sc.clientWidth : 0;
    });
    if (width <= 900) assert.ok(scrolls > 0, `${width}px: the table should scroll sideways, not clip`);
  }
  // The two response-side cards share a row once there is room, and a row of
  // two cards only reads as a row if they are squared off. The probe's own
  // pairs have to line up as well: the side column sits level with the proxy
  // box it describes, not with that box's caption.
  await page.setViewportSize({width:1440,height:1100});
  await page.evaluate(()=>setWorkspace('history',false));
  await page.waitForTimeout(150);
  const paired = await page.evaluate(()=>{
    const box = sel => {const r=document.querySelector(sel).getBoundingClientRect(); return {t:Math.round(r.top),h:Math.round(r.height)};};
    const deg=box('#degradedPanel'), probe=box('#probePanel');
    return {sameRow:deg.t===probe.t, equalHeight:deg.h===probe.h, probeWider:box('#probePanel').h>0 &&
      document.querySelector('#probePanel').getBoundingClientRect().width > document.querySelector('#degradedPanel').getBoundingClientRect().width,
      heads:['#degradedPanel','#probePanel'].map(s=>Math.round(document.querySelector(s+' .panel-head').getBoundingClientRect().height))};
  });
  assert.equal(paired.sameRow, true, 'the two cards share a row at 1440px');
  assert.equal(paired.equalHeight, true, 'and are squared off');
  assert.equal(paired.probeWider, true, 'the probe gets the wider column, it has more to say');
  // Two cards side by side only read as one row if their title bars match; the
  // bodies are ordered by what each card is for, not to mirror each other.
  assert.equal(paired.heads[0], paired.heads[1], `title bars differ: ${paired.heads}`);
  // These cards sit above the table that is actually being read, so nothing in
  // them may be set larger than the table's own body text.
  const shouting = await page.evaluate(() => {
    const body = parseFloat(getComputedStyle(document.querySelector('.history-table td')).fontSize);
    return [...document.querySelectorAll('#degradedPanel *,#probePanel *')]
      .filter(el => el.closest('.panel-head') === null && el.closest('.hint-text') === null)
      .filter(el => [...el.childNodes].some(n => n.nodeType === 3 && n.textContent.trim()))
      .filter(el => parseFloat(getComputedStyle(el).fontSize) > body)
      .map(el => `${el.tagName}.${el.className}:${getComputedStyle(el).fontSize}`);
  });
  assert.deepEqual(shouting, [], `settings text larger than the table body: ${JSON.stringify(shouting)}`);
  // Below the breakpoint they go back to stacking rather than squeezing.
  await page.setViewportSize({width:1100,height:1100});
  await page.waitForTimeout(150);
  const stacked = await page.evaluate(()=>document.querySelector('#degradedPanel').getBoundingClientRect().top
    !== document.querySelector('#probePanel').getBoundingClientRect().top);
  assert.equal(stacked, true, 'narrow viewports stack the two cards');

  assert.deepEqual(failures,[]);
  console.log('panel review regressions: passed');
} finally { await browser.close(); }
