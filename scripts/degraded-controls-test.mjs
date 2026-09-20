import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', e => errors.push(e.message));
let rule = {auth_index:'a',enabled:false,set:{'X-Fixture':'keep'},remove:['X-Old'],reject_degraded_response:false,retry_on_degraded:true,retry_attempts:4};
let failSave = false;
await page.route('http://panel.test/**', async route => {
  const url = new URL(route.request().url());
  if (url.pathname.endsWith('/index')) return route.fulfill({contentType:'text/html',body:fs.readFileSync(new URL('../web/index.html',import.meta.url),'utf8')});
  let payload = {};
  if (url.pathname.endsWith('/credentials')) payload = {credentials:[{auth_index:'a',name:'a.json'}]};
  if (url.pathname.endsWith('/rule')) {
    if (route.request().method() === 'PUT') {
      if (failSave) return route.fulfill({status:500,contentType:'application/json',body:'{"error":"save failed"}'});
      rule = route.request().postDataJSON();
    }
    payload = {rule};
  }
  if (url.pathname.endsWith('/history')) payload = {items:[],total:0};
  if (url.pathname.endsWith('/turn-states')) payload = {turn_states:[]};
  await route.fulfill({contentType:'application/json',body:JSON.stringify(payload)});
});
try {
  await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
  await page.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#addSet').isDisabled(),true);
  assert.equal(await page.locator('#setRows .hv').isDisabled(),true);
  assert.equal(await page.locator('#addRemove').isDisabled(),true);
  assert.equal(await page.locator('#ruleStripTurnState').isDisabled(),true);
  await page.evaluate(()=>{ document.querySelector('#ruleEnabled').click(); });
  assert.equal(await page.locator('#setRows .hv').isEnabled(),true);
  await page.evaluate(()=>{ document.querySelector('#ruleEnabled').click(); });
  assert.equal(await page.locator('#setRows .hv').inputValue(),'keep');
  assert.equal(await page.locator('#saveRule').isEnabled(),true);
  await page.evaluate(()=>setWorkspace('history',false));
  assert.equal(await page.locator('#retryDegraded').isDisabled(),true);
  assert.equal(await page.locator('#retryAttempts').isDisabled(),true);
  assert.equal(await page.locator('#rejectDegradedModels').isDisabled(),true);
  assert.equal(await page.locator('#retryProxies').isDisabled(),true);
  assert.equal(await page.locator('label[for="retryAttempts"]').textContent(),'最大重试次数');
  await page.locator('label').filter({has:page.locator('#rejectDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#retryAttempts').disabled);
  assert.equal(await page.locator('#retryDegraded').isEnabled(),true);
  assert.equal(await page.locator('#retryAttempts').inputValue(),'4');
  assert.equal(rule.reject_degraded_response,true);
  assert.equal(rule.enabled,false); // Independent of the request rewriting switch.
  await page.locator('#rejectDegradedModels').fill('gpt-5.6-luna, gpt-6-astra\ngpt-5.6-luna');
  await page.locator('#rejectDegradedModels').blur();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.deepEqual(rule.reject_degraded_models,['gpt-5.6-luna','gpt-6-astra']);
  await page.locator('#retryProxies').fill('socks5://localhost:1080\nsocks5h://user:pass@localhost:1081\nsocks5://localhost:1080');
  await page.locator('#retryProxies').blur();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.deepEqual(rule.retry_proxies,['socks5://localhost:1080','socks5h://user:pass@localhost:1081']);
  await page.locator('label').filter({has:page.locator('#retryDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#retryProxies').isDisabled(),true);
  await page.locator('label').filter({has:page.locator('#retryDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#retryProxies').isEnabled(),true);
  await page.locator('label').filter({has:page.locator('#rejectDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#retryAttempts').isDisabled(),true);
  assert.equal(rule.retry_attempts,4);
  assert.equal(await page.locator('#rejectDegradedModels').isDisabled(),true);
  failSave=true;
  await page.locator('label').filter({has:page.locator('#rejectDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#rejectDegraded').isChecked(),false);
  assert.equal(await page.locator('#retryDegraded').isDisabled(),true);
  assert.equal(await page.locator('#retryAttempts').isDisabled(),true);
  for (const width of [1280,390]) {
    await page.setViewportSize({width,height:900});
    await page.screenshot({path:`/tmp/chr-v014-history-${width}.png`,fullPage:true});
    await page.evaluate(()=>setWorkspace('rules',false));
    await page.screenshot({path:`/tmp/chr-v014-rules-${width}.png`,fullPage:true});
    await page.evaluate(()=>setWorkspace('history',false));
  }
  assert.deepEqual(errors,[]);
  console.log('degraded controls: passed');
} finally { await browser.close(); }
