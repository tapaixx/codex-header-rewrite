import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', e => errors.push(e.message));
const record = {
  id:'rec-1', request_id:'rec-1', auth_index:'a', attempt:1, origin:'live', model:'gpt-6-astra',
  requested_model:'gpt-6-astra', upstream_model:'gpt-6-astra', source_format:'openai-response', stream:true,
  started_at:'2026-09-21T01:00:00Z', completed_at:'2026-09-21T01:00:04Z', outcome:'succeeded', status_code:200,
  before_headers:{'X-Old':['1'],'Authorization':['Bearer [REDACTED]']},
  after_headers:{'X-Old':['2'],'Authorization':['Bearer [REDACTED]']},
  response_headers:{'X-Codex-Turn-State':['abc'],'Set-Cookie':['session=kept']},
};
let bodyCalls = 0;
await page.route('http://panel.test/**', async route => {
  const u = new URL(route.request().url());
  let d = {};
  if (u.pathname.endsWith('/index')) return route.fulfill({contentType:'text/html',body:fs.readFileSync(new URL('../web/index.html',import.meta.url),'utf8')});
  if (u.pathname.endsWith('/credentials')) d = {credentials:[{auth_index:'a',name:'a.json'}]};
  if (u.pathname.endsWith('/rule')) d = {rule:{auth_index:'a',enabled:true,set:{},remove:[]}};
  if (u.pathname.endsWith('/history/body')) {
    bodyCalls++;
    assert.equal(u.searchParams.get('id'),'rec-1');
    d = {id:'rec-1',found:true,masked_request_field:'input',masked_response_field:'output',max_stored_bytes:262144,
         request_body:'{"model":"gpt-6-astra","input":"[MASKED 4096 bytes]","tools":[]}', request_bytes:400000,
         response_body:'event: response.completed\ndata: {"type":"response.completed","sequence_number":13,"stream":true,"response":{"model":"gpt-6-astra","output":"[MASKED 88 bytes]"}}\n\n',
         response_bytes:1024};
  } else if (u.pathname.endsWith('/history')) d = {total:1,page:1,total_pages:1,items:[record]};
  if (u.pathname.endsWith('/turn-states')) d = {turn_states:[],reuse_window_seconds:200};
  await route.fulfill({contentType:'application/json',body:JSON.stringify(d)});
});
try {
  await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
  await page.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
  await page.evaluate(()=>setWorkspace('history',false));
  await page.waitForSelector('.history-summary');
  await page.click('.history-summary');
  await page.waitForSelector('.detail-tabs');

  // Five tabs, in the order asked for, first one selected.
  const labels = await page.locator('.detail-tab').allTextContents();
  assert.deepEqual(labels.map(t=>t.replace(/\d+$/,'').trim()),
    ['X-Codex-Turn-State','Request Header','Request Body','Response Header','Response Body'], JSON.stringify(labels));
  assert.equal(await page.locator('.detail-tab').first().getAttribute('aria-selected'),'true');
  assert.equal(await page.locator('#detailPane-state').isVisible(), true);
  assert.equal(await page.locator('#detailPane-reqb').isVisible(), false);

  await page.waitForFunction(()=>!document.querySelector('#detailRequestBody').textContent.includes('读取中'));
  assert.equal(bodyCalls,1,'the body is fetched once per record');

  // Request Body: formatted, masked field visible, truncation flagged.
  await page.locator('.detail-tab[data-pane="reqb"]').click();
  assert.equal(await page.locator('#detailPane-reqb').isVisible(), true);
  assert.equal(await page.locator('#detailPane-state').isVisible(), false);
  const reqText = await page.locator('#detailPane-reqb .body-view').textContent();
  assert.ok(reqText.includes('\n  "model": "gpt-6-astra"'), 'json is pretty printed: '+JSON.stringify(reqText.slice(0,80)));
  assert.ok(reqText.includes('[MASKED 4096 bytes]'), 'the masked field is shown as masked');
  const reqMeta = await page.locator('#detailPane-reqb .body-meta').textContent();
  assert.ok(reqMeta.includes('input') && reqMeta.includes('已遮蔽'), reqMeta);
  assert.ok(reqMeta.includes('已截断'), 'a 400 KB body over the cap is flagged: '+reqMeta);

  // Response Body: SSE framing kept, each data payload formatted.
  await page.locator('.detail-tab[data-pane="resb"]').click();
  const resText = await page.locator('#detailPane-resb .body-view').textContent();
  assert.ok(resText.startsWith('event: response.completed'), 'sse framing survives: '+JSON.stringify(resText.slice(0,40)));
  assert.ok(resText.includes('\n  "type": "response.completed"'), 'the data payload is formatted');
  assert.ok(!(await page.locator('#detailPane-resb .body-meta').textContent()).includes('已截断'), 'a 1 KB body is not truncated');

  // Bodies are syntax coloured: keys, strings, numbers, and the masked value
  // called out on its own.
  const classes = await page.$$eval('#detailPane-resb .body-view span', els => [...new Set(els.map(e=>e.className))].sort());
  assert.deepEqual(classes, ['jb','jk','jm','jn','jp','js'], JSON.stringify(classes));

  // A stream whose framing the host dropped is put back on separate lines for
  // display, and still coloured.
  await page.evaluate(()=>{
    document.querySelector('#detailResponseBody').innerHTML =
      bodyPaneHTML('Response Body', 'event: response.createddata: {"type":"response.created","response":{"model":"gpt-6-astra"}}', 120, 'output', 262144);
  });
  const unframed = await page.locator('#detailPane-resb .body-view').textContent();
  assert.ok(unframed.startsWith('event: response.created\ndata: {'), 'framing restored: '+JSON.stringify(unframed.slice(0,40)));
  assert.ok((await page.locator('#detailPane-resb .body-view .jk').count()) > 0, 'still coloured');

  // Keyboard moves along the strip.
  await page.locator('.detail-tab[data-pane="resb"]').press('ArrowRight');
  assert.equal(await page.locator('.detail-tab[data-pane="state"]').getAttribute('aria-selected'),'true','wraps to the first');
  await page.locator('.detail-tab[data-pane="state"]').press('End');
  assert.equal(await page.locator('.detail-tab[data-pane="resb"]').getAttribute('aria-selected'),'true');

  // Header panes still hold what they held before the tabs existed.
  await page.locator('.detail-tab[data-pane="resh"]').click();
  assert.ok((await page.locator('#detailPane-resh').textContent()).includes('X-Codex-Turn-State'));
  await page.locator('.detail-tab[data-pane="reqh"]').click();
  assert.ok((await page.locator('#detailPane-reqh').textContent()).includes('X-Old'));

  assert.deepEqual(errors,[]);
  console.log('detail tabs: passed');
} finally { await browser.close(); }
