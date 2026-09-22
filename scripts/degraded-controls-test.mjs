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
  // The injection switch is the pool's, not the rule's: a rule that is off
  // leaves it live.
  assert.equal(await page.locator('#poolInject').isDisabled(),false);
  await page.evaluate(()=>{ document.querySelector('#ruleEnabled').click(); });
  assert.equal(await page.locator('#setRows .hv').isEnabled(),true);
  await page.evaluate(()=>{ document.querySelector('#ruleEnabled').click(); });
  assert.equal(await page.locator('#setRows .hv').inputValue(),'keep');
  assert.equal(await page.locator('#saveRule').isEnabled(),true);
  await page.evaluate(()=>setWorkspace('history',false));
  assert.equal(await page.locator('#retryDegraded').isDisabled(),true);
  assert.equal(await page.locator('#retryAttempts').isDisabled(),true);
  assert.equal(await page.locator('#rejectDegradedModels').getAttribute('data-disabled'),'1');
  assert.equal(await page.locator('#retryProxies').getAttribute('data-disabled'),'1');
  assert.equal(await page.locator('label[for="retryAttempts"]').textContent(),'最大重试次数');
  await page.locator('label').filter({has:page.locator('#rejectDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#retryAttempts').disabled);
  assert.equal(await page.locator('#retryDegraded').isEnabled(),true);
  assert.equal(await page.locator('#retryAttempts').inputValue(),'4');
  assert.equal(rule.reject_degraded_response,true);
  assert.equal(rule.enabled,false); // Independent of the request rewriting switch.
  await page.locator('#rejectDegradedModels .chip-add').fill('gpt-5.6-luna, gpt-6-astra, gpt-5.6-luna');
  await page.locator('#rejectDegradedModels .chip-add').press('Enter');
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.deepEqual(rule.reject_degraded_models,['gpt-5.6-luna','gpt-6-astra']);
  // The proxy list is only editable once its own switch is on; off, retries go direct.
  assert.equal(await page.locator('#retryProxies').getAttribute('data-disabled'),'1');
  await page.evaluate(()=>document.querySelector('#retryProxyOn').click());
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(rule.retry_proxy_enabled,true);
  await page.locator('#retryProxies .chip-add').fill('socks5://localhost:1080 socks5h://user:pass@localhost:1081 socks5://localhost:1080');
  await page.locator('#retryProxies .chip-add').press('Enter');
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.deepEqual(rule.retry_proxies,['socks5://localhost:1080','socks5h://user:pass@localhost:1081']);
  // The password is masked on screen; focusing that chip reveals it.
  assert.equal(await page.locator('#retryProxies .chip-field').nth(1).inputValue(),'socks5h://user:***@localhost:1081');
  await page.locator('#retryProxies .chip-field').nth(1).focus();
  assert.equal(await page.locator('#retryProxies .chip-field').nth(1).inputValue(),'socks5h://user:pass@localhost:1081');
  assert.equal(await page.locator('#rejectDegradedModels .chip-tag').count(),2);
  // A duplicate is refused rather than merged in silence.
  await page.locator('#rejectDegradedModels .chip-add').fill('gpt-6-astra');
  await page.locator('#rejectDegradedModels .chip-add').press('Enter');
  assert.equal(await page.locator('#rejectDegradedModels .chip-tag').count(),2);
  assert.match(await page.locator('.toast').textContent(),/已存在/);
  await page.locator('label').filter({has:page.locator('#retryDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#retryProxies').getAttribute('data-disabled'),'1');
  await page.locator('label').filter({has:page.locator('#retryDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#retryProxies').getAttribute('data-disabled'),'');
  await page.locator('label').filter({has:page.locator('#rejectDegraded')}).click();
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#retryAttempts').isDisabled(),true);
  assert.equal(rule.retry_attempts,4);
  assert.equal(await page.locator('#rejectDegradedModels').getAttribute('data-disabled'),'1');
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
