import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', e => errors.push(e.message));
let rule = {auth_index:'a',enabled:true,set:{},remove:[],inject_turn_state:true,state_ttl_seconds:0};
let saved = null;
let poolWindow = 200;
await page.route('http://panel.test/**', async route => {
  const url = new URL(route.request().url());
  if (url.pathname.endsWith('/index')) return route.fulfill({contentType:'text/html',body:fs.readFileSync(new URL('../web/index.html',import.meta.url),'utf8')});
  let payload = {};
  if (url.pathname.endsWith('/credentials')) payload = {credentials:[{auth_index:'a',name:'a.json'}]};
  if (url.pathname.endsWith('/rule')) {
    if (route.request().method() === 'PUT') { saved = route.request().postDataJSON(); rule = saved; }
    payload = {rule};
  }
  if (url.pathname.endsWith('/history')) payload = {items:[],total:0};
  if (url.pathname.endsWith('/turn-states')) payload = {reuse_window_seconds:poolWindow, turn_states:[
    {state:'x', digest:'d1', auth_index:'a', label:'A', model:'gpt-5.6-luna', plan_type:'team', chars:332, max_chars:332,
     minted_at:new Date(Date.now()-45000).toISOString(), age_seconds:45, expired:false, reuse_window_seconds:poolWindow},
    {state:'y', digest:'d2', auth_index:'a', label:'A', model:'gpt-5.6', plan_type:'team', chars:332, max_chars:332,
     minted_at:new Date(Date.now()-900000).toISOString(), age_seconds:900, expired:true, reuse_window_seconds:poolWindow}]};
  await route.fulfill({contentType:'application/json',body:JSON.stringify(payload)});
});
try {
  await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
  await page.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
  await page.waitForFunction(()=>!document.querySelector('#ruleEnabled').disabled);

  // The control lives on the pool panel now, which renders in the history
  // workspace, so that is where it is exercised.
  await page.evaluate(()=>setWorkspace('history',false));
  await page.waitForFunction(()=>document.querySelectorAll('.pool-row').length===2);

  // A rule with no value shows the default.
  assert.equal(await page.locator('#ruleStateTTL').inputValue(), '200', 'zero shows the default');

  // The window belongs to the pool it governs, not to the rewrite rule, so it
  // no longer follows 启用改写 -- the same way the response-side settings do
  // not. It is live whenever a rule is loaded.
  assert.equal(await page.locator('#turnStatePanel #ruleStateTTL').count(), 1, 'the control lives on the pool panel');
  assert.equal(await page.locator('#rulePanel #ruleStateTTL').count(), 0, 'and no longer in the rule editor');
  await page.evaluate(()=>document.querySelector('#ruleEnabled').click());
  assert.equal(await page.locator('#ruleStateTTL').isDisabled(), false, 'independent of 启用改写');
  await page.evaluate(()=>document.querySelector('#ruleEnabled').click());

  // It saves itself on change; there is no second save button for one field.
  await page.fill('#ruleStateTTL','45');
  await page.dispatchEvent('#ruleStateTTL','change');
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.state_ttl_seconds, 45, `sent ${JSON.stringify(saved && saved.state_ttl_seconds)}`);

  // Out of range is clamped in the field before it can be refused.
  await page.fill('#ruleStateTTL','99999');
  await page.dispatchEvent('#ruleStateTTL','change');
  assert.equal(await page.locator('#ruleStateTTL').inputValue(), '7200');
  await page.fill('#ruleStateTTL','1');
  await page.dispatchEvent('#ruleStateTTL','change');
  assert.equal(await page.locator('#ruleStateTTL').inputValue(), '5');
  // Blank means "use the default", which the plugin spells as zero.
  await page.fill('#ruleStateTTL','');
  await page.dispatchEvent('#ruleStateTTL','change');
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.state_ttl_seconds, 0, 'blank sends zero');

  // The pool header states the credential's own window, in seconds when short.
  assert.equal(await page.locator('#turnStateWindow').textContent(), '200 秒');
  const ages = await page.locator('.pool-row td.num:nth-child(4)').allTextContents();
  assert.ok(ages[0].endsWith('45 秒前'), `sub-minute age reads in seconds: ${ages[0]}`);
  assert.ok(ages[1].endsWith('15 分钟前'), `older age reads in minutes: ${ages[1]}`);
  assert.equal((await page.locator('.pool-row').nth(1).textContent()).includes('已过期'), true);

  // A longer window is reported in minutes.
  poolWindow = 1800;
  await page.evaluate(()=>loadTurnStates());
  await page.waitForFunction(()=>document.querySelector('#turnStateWindow').textContent==='30 分钟');

  // The pool's two switches ride its title bar. The master switch defaults on,
  // the injection switch to whatever the rule says, and neither has anything
  // to do with 启用改写 or with 保存规则: each saves itself on change.
  assert.equal(await page.locator('#poolMaintain').isChecked(), true, 'maintenance defaults on');
  assert.equal(await page.locator('#poolInject').isChecked(), true, 'injection reads the rule');
  assert.equal(await page.locator('#turnStatePanel .panel-head #poolInject').count(), 1, 'the switch lives on the pool card');
  assert.equal(await page.locator('#rulePanel #ruleStripTurnState').count(), 0, 'and no longer in the rule editor');
  await page.evaluate(()=>document.querySelector('#ruleEnabled').click());
  assert.equal(await page.locator('#poolInject').isDisabled(), false, 'independent of 启用改写');
  await page.evaluate(()=>document.querySelector('#ruleEnabled').click());
  await page.evaluate(()=>document.querySelector('#poolInject').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.inject_turn_state, false, 'turning injection off saved at once');
  assert.equal(saved.maintain_state_pool, true, 'and carried the master switch as on');
  // Freezing the pool takes the injection switch with it.
  await page.evaluate(()=>document.querySelector('#poolMaintain').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.maintain_state_pool, false, 'freezing saved');
  assert.equal(await page.locator('#poolInject').isDisabled(), true, 'a frozen pool cannot inject');
  assert.equal(await page.locator('#poolCookie').isDisabled(), true, 'nor inject a cookie');
  await page.evaluate(()=>document.querySelector('#poolMaintain').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(await page.locator('#poolInject').isDisabled(), false, 'thawed, it is back');

  // The cookie switch sits beside the state switch and saves on its own; the
  // two do not move each other.
  assert.equal(await page.locator('#turnStatePanel .panel-head #poolCookie').count(), 1, 'the cookie switch lives on the pool card');
  assert.equal(await page.locator('#poolCookie').isChecked(), false, 'cookie injection defaults off');
  const cookieLight = page.locator('#statusLights .light[data-light="cookie"]');
  assert.equal(await cookieLight.getAttribute('data-on'), '0', 'its light is off');
  await page.evaluate(()=>document.querySelector('#poolCookie').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.inject_cookie, true, 'turning it on saved at once');
  assert.equal(saved.inject_turn_state, false, 'without touching state injection');
  assert.equal(await cookieLight.getAttribute('data-on'), '1', 'and its light came on');
  await page.evaluate(()=>document.querySelector('#saveRule').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.inject_cookie, true, 'saving the header rule keeps it');

  assert.deepEqual(errors, []);
  console.log('state ttl panel: ok');
} finally { await browser.close(); }
