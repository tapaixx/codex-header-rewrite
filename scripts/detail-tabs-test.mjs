import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', e => errors.push(e.message));
// A routing JWT the way __oailb carries one: three base64url segments, unsigned here.
const b64url = (obj) => Buffer.from(JSON.stringify(obj)).toString('base64').replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
const oailb = `${b64url({alg:'HS256',typ:'JWT'})}.${b64url({host:'gw-iad-7.internal',iss:'oai-lb',aud:'chatgpt.com',iat:1758556200,exp:1758563400})}.${b64url({sig:'x'})}`;
const record = {
  id:'rec-1', request_id:'rec-1', auth_index:'a', attempt:1, origin:'live', model:'gpt-6-astra',
  requested_model:'gpt-6-astra', upstream_model:'gpt-6-astra', source_format:'openai-response', stream:true,
  request_effort:'xhigh', upstream_effort:'low', model_mismatch:false,
  started_at:'2026-09-21T01:00:00Z', completed_at:'2026-09-21T01:00:04Z', outcome:'succeeded', status_code:200,
  before_headers:{'X-Old':['1'],'Authorization':['Bearer [REDACTED]']},
  after_headers:{'X-Old':['2'],'Authorization':['Bearer [REDACTED]'],'Cookie':[`__oailb=${oailb}; __cf_bm=abc-1758556800-1.0.1.1-sig; plain=1`]},
  // One Set-Cookie collapsed by a proxy, with a comma inside Expires that must not split it.
  response_headers:{'X-Codex-Turn-State':['abc'],'Set-Cookie':['session=kept; Path=/; Max-Age=7200; Secure; HttpOnly, __cflb=lb1; expires=Wed, 21 Oct 2026 07:28:00 GMT; SameSite=None']},
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
    d = {id:'rec-1',found:true,masked_request_fields:['input'],masked_response_fields:['output','tools','usage'],max_stored_bytes:262144,
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
  await page.waitForSelector('#drawerBody .detail-tabs');

  // Five tabs, analysis first and selected.
  const labels = await page.locator('#drawerBody .detail-tab').allTextContents();
  assert.deepEqual(labels.map(t=>t.replace(/\d+$/,'').trim()),
    ['分析','Request Header','Request Body','Response Header','Response Body'], JSON.stringify(labels));
  assert.equal(await page.locator('#drawerBody .detail-tab').first().getAttribute('aria-selected'),'true');
  assert.equal(await page.locator('#detailPane-analysis').isVisible(), true);
  assert.equal(await page.locator('#detailPane-reqh').isVisible(), false);
  assert.equal(await page.locator('#detailPane-reqb').isVisible(), false);

  // The analysis tab holds the turn-state cards and, under them, the cookie
  // cards; nothing analytical sits above the strip any more.
  const stateBlock = page.locator('#detailPane-analysis .history-detail-section').filter({hasText:'X-Codex-Turn-State'}).first();
  assert.equal(await stateBlock.isVisible(), true, 'turn state is on the analysis tab');
  assert.equal(await page.locator('.history-detail-shell > .history-detail-section').filter({hasText:'X-Codex-Turn-State'}).count(), 0,
    'and not above the strip');
  const cookieBlock = page.locator('#detailPane-analysis .history-detail-section').filter({hasText:'Cookie'}).last();
  assert.equal(await cookieBlock.isVisible(), true, 'the cookie section renders on the same tab');
  const order = await page.evaluate(() => {
    const blocks = [...document.querySelectorAll('#detailPane-analysis .history-detail-section')];
    const state = blocks.findIndex((el) => el.textContent.includes('X-Codex-Turn-State'));
    const cookie = blocks.findIndex((el) => el.querySelector('.history-detail-label')?.textContent.startsWith('Cookie'));
    return { state, cookie };
  });
  assert.ok(order.state >= 0 && order.cookie > order.state, 'state cards come before the cookie cards: '+JSON.stringify(order));
  const cookieText = (await cookieBlock.textContent()).replace(/\s+/g,' ');
  assert.ok(cookieText.includes('OpenAI 路由 JWT') && cookieText.includes('gw-iad-7.internal') && cookieText.includes('oai-lb'),
    'a JWT-shaped cookie is decoded and its routing claims shown: '+cookieText.slice(0,200));
  assert.ok(cookieText.includes('仅解码，未验签'), 'and says it did not verify the signature');
  assert.ok(cookieText.includes('内嵌时间'), 'the __cf_bm timestamp is read out');
  assert.ok(cookieText.includes('7200 秒 · 2 小时') && cookieText.includes('Expires'), 'Set-Cookie attributes are parsed');
  assert.equal(await cookieBlock.locator('.ts-block').nth(1).locator('.ts-sub').count(), 2, 'a collapsed Set-Cookie splits into two cookies, not at the comma in Expires');
  assert.ok(!cookieText.includes('demo-session-value') && !cookieText.includes('kept'), 'raw cookie values are not printed');
  await page.locator('#drawerBody .detail-tab[data-pane="reqh"]').click();

  await page.waitForFunction(()=>!document.querySelector('#detailRequestBody').textContent.includes('读取中'));
  assert.equal(bodyCalls,1,'the body is fetched once per record');

  // Request Body: formatted, masked field visible, truncation flagged.
  await page.locator('#drawerBody .detail-tab[data-pane="reqb"]').click();
  assert.equal(await page.locator('#detailPane-reqb').isVisible(), true);
  assert.equal(await page.locator('#detailPane-state').isVisible(), false);
  const reqText = await page.locator('#detailPane-reqb .body-view').textContent();
  assert.ok(reqText.includes('"model": "gpt-6-astra"'), 'json is formatted: '+JSON.stringify(reqText.slice(0,80)));
  assert.ok(reqText.includes('[MASKED 4096 bytes]'), 'the masked field is shown as masked');
  // The body is a tree of native details, so it folds.
  assert.ok((await page.locator('#detailPane-reqb details.jnode').count()) > 0, 'the body renders as a tree');
  const reqMeta = await page.locator('#detailPane-reqb .body-meta').textContent();
  assert.ok(reqMeta.includes('input') && reqMeta.includes('已遮蔽'), reqMeta);
  assert.ok(reqMeta.includes('已截断'), 'a 400 KB body over the cap is flagged: '+reqMeta);

  // Response Body: SSE framing kept, each data payload formatted.
  await page.locator('#drawerBody .detail-tab[data-pane="resb"]').click();
  const resText = await page.locator('#detailPane-resb .body-view').textContent();
  assert.ok(resText.startsWith('event: response.completed'), 'sse framing survives: '+JSON.stringify(resText.slice(0,40)));
  assert.ok(resText.includes('"type": "response.completed"'), 'the data payload is formatted');

  // Expand and collapse act on the pane they belong to, and folding hides
  // what was folded.
  const open = () => page.locator('#detailPane-resb details.jnode[open]').count();
  const height = () => page.evaluate(() => Math.round(document.querySelector('#detailPane-resb .body-view').scrollHeight));
  const all = await page.locator('#detailPane-resb details.jnode').count();
  assert.ok(all > 0, 'the stream renders as trees');
  const tall = await height();
  await page.locator('#detailPane-resb [data-body-act="collapse"]').click();
  assert.equal(await open(), 0, 'collapse closes every node');
  const short = await height();
  assert.ok(short < tall, `folding should shorten the pane: ${short} vs ${tall}`);
  await page.locator('#detailPane-resb [data-body-act="expand"]').click();
  assert.equal(await open(), all, 'expand opens every node');
  assert.equal(await height(), tall, 'expanding restores the height');
  // Indentation must not leave blank lines: whitespace between the tags of a
  // details element renders as text.
  assert.equal(await page.evaluate(() => {
    let n = 0;
    for (const d of document.querySelectorAll('#detailPane-resb details.jnode')) {
      for (const c of d.childNodes) if (c.nodeType === 3 && c.textContent.length && !c.textContent.trim()) n += 1;
    }
    return n;
  }), 0, 'stray whitespace nodes inside details');
  assert.ok(!(await page.locator('#detailPane-resb .body-meta').textContent()).includes('已截断'), 'a 1 KB body is not truncated');

  // Bodies are syntax coloured: keys, strings, numbers, and the masked value
  // called out on its own.
  const classes = await page.$$eval('#detailPane-resb .body-view span', els => [...new Set(els.map(e=>e.className))].sort());
  for (const want of ['jk','jm','jn','jp','js']) assert.ok(classes.includes(want), `missing ${want} in ${JSON.stringify(classes)}`);

  // A stream whose framing the host dropped is put back on separate lines for
  // display, and still coloured.
  await page.evaluate(()=>{
    document.querySelector('#detailResponseBody').innerHTML =
      bodyPaneHTML('Response Body', 'event: response.createddata: {"type":"response.created","response":{"model":"gpt-6-astra"}}', 120, ['output','tools'], 262144);
  });
  // The framing is put back as its own rows rather than as literal newlines,
  // so read the rows.
  const rows = await page.locator('#detailPane-resb .body-view .jevent').allTextContents();
  assert.deepEqual(rows.map((r) => r.trim()), ['event: response.created', 'data:'], JSON.stringify(rows));
  assert.ok((await page.locator('#detailPane-resb .body-view .jk').count()) > 0, 'still coloured');
  assert.ok((await page.locator('#detailPane-resb .body-view details.jnode').count()) > 0, 'and foldable');

  // The analysis pane is hidden while a body tab is open.
  assert.equal(await stateBlock.isVisible(), false, 'turn state hidden on the body tab');

  // Keyboard moves along the strip.
  await page.locator('#drawerBody .detail-tab[data-pane="resb"]').press('ArrowRight');
  assert.equal(await page.locator('#drawerBody .detail-tab[data-pane="analysis"]').getAttribute('aria-selected'),'true','wraps to the first');
  assert.equal(await stateBlock.isVisible(), true, 'and the turn state is back');
  await page.locator('#drawerBody .detail-tab[data-pane="analysis"]').press('End');
  assert.equal(await page.locator('#drawerBody .detail-tab[data-pane="resb"]').getAttribute('aria-selected'),'true','End lands on the last tab');

  // Header panes still hold what they held before the tabs existed.
  await page.locator('#drawerBody .detail-tab[data-pane="resh"]').click();
  assert.ok((await page.locator('#detailPane-resh').textContent()).includes('X-Codex-Turn-State'));
  await page.locator('#drawerBody .detail-tab[data-pane="reqh"]').click();
  assert.ok((await page.locator('#detailPane-reqh').textContent()).includes('X-Old'));
  // The unchanged headers open with the pane. They were folded away when the
  // detail was one long column and this block sat below everything else; on a
  // tab of its own there is nothing to scroll past.
  const unchanged = page.locator('#detailPane-reqh details');
  assert.equal(await unchanged.count(), 1);
  assert.equal(await unchanged.evaluate((el) => el.open), true, 'unchanged headers start open');
  assert.equal(await page.locator('#detailPane-reqh details .diff-row').first().isVisible(), true);

  // The reasoning effort rides with each model, a size down, and takes no part
  // in the verdict: these two disagree and the models still read as a match.
  const efforts = await page.locator('.history-detail-head .chain .effort').allTextContents();
  assert.deepEqual(efforts, ['xhigh','low'], JSON.stringify(efforts));
  const sizes = await page.$$eval('.history-detail-head .chain .effort',
    els => els.map(e => [parseFloat(getComputedStyle(e).fontSize), parseFloat(getComputedStyle(e.parentElement).fontSize)]));
  for (const [own, parent] of sizes) assert.ok(own < parent, `effort ${own}px should be smaller than ${parent}px`);
  assert.ok((await page.locator('.history-detail-head .chain').textContent()).includes('一致'),
    'differing efforts must not read as a model mismatch');

  // A payload whose tool description contains the literal "data:" must still
  // format. Splitting on the marker cut the JSON string in half and nothing
  // after it formatted at all.
  const tricky = 'event: response.created\ndata: {"type":"response.created","response":{"model":"gpt-6-astra",'
    + '"tools":[{"name":"view_image","description":"image_url should be a base64-encoded `data:` URL, and an `event:` block too"}]}}\n\n';
  const rendered = await page.evaluate((body) => {
    const host = document.createElement('div');
    host.innerHTML = renderBody(body);
    return {
      text: host.textContent,
      keys: [...host.querySelectorAll('.jk')].map((e) => e.textContent),
      nodes: host.querySelectorAll('details.jnode').length,
    };
  }, tricky);
  assert.ok(rendered.nodes > 0, 'the payload became a tree: '+JSON.stringify(rendered.text.slice(0,80)));
  for (const key of ['"type"', '"response"', '"model"', '"tools"']) {
    assert.ok(rendered.keys.includes(key), `key ${key} missing from ${JSON.stringify(rendered.keys)}`);
  }
  assert.ok(rendered.text.includes('base64-encoded `data:` URL'), 'the literal marker survives inside the string');

  assert.deepEqual(errors,[]);
  console.log('detail tabs: passed');
} finally { await browser.close(); }
