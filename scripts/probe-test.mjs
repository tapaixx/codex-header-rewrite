// The probe is a background task that spends quota on a schedule, so the panel
// has to make three things visible before it is switched on: which models it
// will keep warm, which exit it will use, and what that costs.
import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', e => errors.push(e.message));
let rule = {auth_index:'a',enabled:true,set:{},remove:[],inject_turn_state:true,
  reject_degraded_response:true,retry_on_degraded:true,retry_proxies:['socks5://127.0.0.1:9050']};
let saved = null;
await page.route('http://panel.test/**', async route => {
  const url = new URL(route.request().url());
  if (url.pathname.endsWith('/index')) return route.fulfill({contentType:'text/html',body:fs.readFileSync(new URL('../web/index.html',import.meta.url),'utf8')});
  let payload = {};
  if (url.pathname.endsWith('/credentials')) payload = {credentials:[{auth_index:'a',name:'a.json'}]};
  if (url.pathname.endsWith('/rule')) {
    if (route.request().method() === 'PUT') { saved = route.request().postDataJSON(); rule = {...saved}; }
    payload = {rule};
  }
  if (url.pathname.endsWith('/probes')) payload = {auth_index:'a',page:1,page_size:10,total:0,total_pages:0,items:[],
    limit:500, enabled:!!rule.probe_enabled, within_window:true};
  else if (url.pathname.endsWith('/history')) payload = {items:[],total:0};
  if (url.pathname.endsWith('/turn-states')) payload = {turn_states:[],reuse_window_seconds:200};
  await route.fulfill({contentType:'application/json',body:JSON.stringify(payload)});
});
try {
  await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
  await page.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
  await page.waitForFunction(()=>!document.querySelector('#ruleEnabled').disabled);
  await page.evaluate(()=>setWorkspace('history',false));
  await page.waitForSelector('#probePanel .probe-grid');

  // Off, everything is still editable: the models are required before the
  // probe may be switched on, so they cannot wait for the switch. Switching
  // on without a model is refused on the spot rather than by a failed save.
  assert.equal(await page.locator('#probeEnabled').isChecked(), false);
  for (const id of ['probeInterval','probeCookieTTL','probeWindowStart','probeWindowEnd']) {
    assert.equal(await page.locator(`#${id}`).isDisabled(), false, `${id} editable while off`);
  }
  assert.equal(await page.locator('#probeModels').getAttribute('data-disabled'), '');
  saved = null;
  await page.evaluate(()=>document.querySelector('#probeEnabled').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(await page.locator('#probeEnabled').isChecked(), false, 'no models, no probe');
  assert.equal(saved, null, 'and nothing was sent');
  assert.match(await page.locator('.toast').textContent(), /探针模型/);
  await page.evaluate(()=>{ setChips('probeModels',['m1']); });

  // Turning it on turns the retry off: both fill the same pool on their own
  // schedule, and two writers for one pool is the thing to prevent.
  assert.equal(await page.locator('#retryDegraded').isChecked(), true, 'retry starts on');
  await page.evaluate(()=>document.querySelector('#probeEnabled').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(await page.locator('#retryDegraded').isChecked(), false, 'the retry was switched off');
  assert.equal(saved.probe_enabled, true);
  assert.equal(saved.retry_on_degraded, false, 'and saved off, not just unticked');

  // Multi-check is off by default and saves on its own; on, the cost line
  // counts the check request.
  assert.equal(await page.locator('#probeVerify').isChecked(), false, 'multi-check defaults off');
  assert.equal(await page.locator('#probeVerify').isDisabled(), false, 'and is editable');
  const before = await page.locator('#probeMeter').textContent();
  await page.evaluate(()=>document.querySelector('#probeVerify').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.probe_verify, true, 'turning it on saved at once');
  assert.equal(saved.probe_enabled, true, 'without touching the probe switch');
  const after = await page.locator('#probeMeter').textContent();
  assert.ok(after.includes('复核请求') && after !== before, 'the rate counts the check: ' + after);
  await page.evaluate(()=>document.querySelector('#probeVerify').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.probe_verify, false, 'and off again');

  // ...and the reverse.
  await page.evaluate(()=>document.querySelector('#retryDegraded').click());
  await new Promise(r=>setTimeout(r,300));
  assert.equal(await page.locator('#probeEnabled').isChecked(), false, 'the probe was switched off');
  await page.evaluate(()=>document.querySelector('#probeEnabled').click());
  await new Promise(r=>setTimeout(r,300));

  // The cookie question only exists once an exit does.
  assert.equal(await page.locator('#probeCookieField').isVisible(), false, 'no proxies, no question');
  await page.evaluate(()=>{ setChips('probeProxies',['socks5://127.0.0.1:1080']); syncProbeControls(); });
  assert.equal(await page.locator('#probeCookieField').isVisible(), false, 'a list with its switch off is not an exit');
  await page.evaluate(()=>{ document.getElementById('probeProxyOn').checked = true; syncProbeControls(); });
  assert.equal(await page.locator('#probeCookieField').isVisible(), true, 'switched on, the proxies raise it');

  // A window is written in browser time and stored in UTC minutes.
  await page.fill('#probeWindowStart','09:00');
  await page.fill('#probeWindowEnd','18:00');
  await page.dispatchEvent('#probeWindowEnd','change');
  // The window saves once the picker rests, not on every tick.
  await new Promise(r=>setTimeout(r,900));
  const expected = (hhmm) => page.evaluate((v)=>localTimeToUTCMinute(v), hhmm);
  assert.equal(saved.probe_window_start_minute, await expected('09:00'), 'start stored in UTC minutes');
  assert.equal(saved.probe_window_end_minute, await expected('18:00'));
  assert.ok((await page.locator('#probeWindowHint').textContent()).startsWith('UTC '), 'the UTC equivalent is shown');

  // Clearing it means all day, which is one state, not two half-filled bounds.
  await page.locator('#probeWindowClear').click();
  await new Promise(r=>setTimeout(r,300));
  assert.equal(saved.probe_window_start_minute, 0);
  assert.equal(saved.probe_window_end_minute, 0);
  assert.equal((await page.locator('#probeWindowHint').textContent()).trim(), '全天生效');

  // The rate is shown because the cost is otherwise invisible, and a rotating
  // pool doubles it.
  await page.evaluate(()=>{ setChips('probeModels',['m1','m2']); syncProbeControls(); });
  await page.fill('#probeInterval','30');
  await page.evaluate(()=>{ document.querySelector('input[name=probeCookieMode][value=static_proxy]').click(); });
  await new Promise(r=>setTimeout(r,300));
  const single = await page.locator('#probeMeter').textContent();
  assert.ok(single.includes('240'), `2 models at 30s is 240/h: ${single}`);
  await page.evaluate(()=>{ document.querySelector('input[name=probeCookieMode][value=rotating_proxy]').click(); });
  await new Promise(r=>setTimeout(r,300));
  const doubled = await page.locator('#probeMeter').textContent();
  assert.ok(doubled.includes('480') && doubled.includes('预热'), `rotating doubles it: ${doubled}`);
  assert.equal(await page.locator('#probeResolveIP').isDisabled(), true, 'rotating cannot resolve an exit');

  // Each mode's reasoning moved behind a marker that sits inside the mode's own
  // label, so reading about a mode must not be the same gesture as choosing it.
  await page.locator('.probe-mode:nth-child(1) .hint').click();
  await new Promise(r=>setTimeout(r,250));
  assert.equal(await page.locator('input[name=probeCookieMode][value=rotating_proxy]').isChecked(), true,
    'reading a mode did not select it');
  assert.ok((await page.locator('.hint-layer').textContent()).includes('真实请求'), 'and the hint opened');
  await page.keyboard.press('Escape');
  await page.locator('.probe-mode:nth-child(1) > span:first-of-type').click();
  await new Promise(r=>setTimeout(r,300));
  assert.equal(await page.locator('input[name=probeCookieMode][value=credential]').isChecked(), true,
    'clicking the card itself still chooses');

  // Copying replaces, because a pool is a set of exits.
  await page.locator('#copyRetryToProbe').click();
  await new Promise(r=>setTimeout(r,300));
  assert.deepEqual(saved.probe_proxies, ['socks5://127.0.0.1:9050'], 'retry pool copied over');

  // The history area separates the two lists, and clearing follows the open one.
  const tabs = await page.locator('#historyPanel .detail-tabs [data-history]').allTextContents();
  assert.deepEqual(tabs.map(t=>t.replace(/\d+$/,'').trim()), ['请求历史','探针历史']);
  await page.locator('[data-history="probe"]').click();
  await page.waitForFunction(()=>document.querySelector('[data-history="probe"]').getAttribute('aria-selected')==='true');
  assert.equal(await page.locator('#historyHeading').textContent(), '历史', 'the head keeps one name; the tabs do the switching');
  assert.equal(await page.locator('#historyPanel .panel-head #probeStatus').count(), 1, 'the schedule sits in the head');
  // The heading flips on the click; the status waits on the fetch behind it, so
  // reading it off the heading alone raced the response about half the time.
  await page.waitForFunction(()=>{
    const el = document.querySelector('#probeStatus');
    return el && !el.hidden && el.textContent.trim() !== '';
  });
  assert.ok((await page.locator('#probeStatus').textContent()).includes('探针已启用'));
  assert.ok((await page.locator('#probeStatus').textContent()).includes('没有真实请求记录'),
    'an account with no traffic is told the probe will not run');

  assert.deepEqual(errors, []);
  console.log('probe panel: passed');
} finally { await browser.close(); }
