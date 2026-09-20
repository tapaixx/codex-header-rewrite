// Small browser smoke test: both themes, all tabs, desktop/mobile and refresh.
import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', error => errors.push(error.message));
let historyReads = 0, poolReads = 0;
await page.route('http://panel.test/**', async route => {
  const path = new URL(route.request().url()).pathname;
  if (path.endsWith('/index')) return route.fulfill({contentType:'text/html',body:fs.readFileSync(new URL('../web/index.html',import.meta.url),'utf8')});
  let data = {};
  if (path.endsWith('/credentials')) data = {plugin_version:'0.14.1',credentials:[{auth_index:'a',name:'a.json',plan_type:'team'}]};
  if (path.endsWith('/rule')) data = {rule:{auth_index:'a',enabled:true,set:{'X-Test':'fixture'},remove:[],reject_degraded_response:true,retry_on_degraded:true}};
  if (path.endsWith('/history')) { historyReads++; data = {items:[],total:0}; }
  if (path.endsWith('/turn-states')) { poolReads++; data = {turn_states:[]}; }
  await route.fulfill({contentType:'application/json',body:JSON.stringify(data)});
});
try {
  await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
  await page.goto('http://panel.test/v0/resource/plugins/codex-header-rewrite/index');
  await page.waitForFunction(()=>!document.querySelector('#rejectDegraded').disabled);
  assert.equal(await page.locator('#historyStats').count(),0,'diagnostics strip should be removed');
  assert.equal(await page.locator('#historyPanel').count(),1,'request history list should remain');
  for (const colorScheme of ['dark','light']) {
    await page.emulateMedia({colorScheme});
    assert.equal(await page.evaluate(()=>getComputedStyle(document.documentElement).colorScheme),colorScheme);
    for (const width of [1280,390]) {
      await page.setViewportSize({width,height:900});
      for (const workspace of ['rules','history','decode']) {
        await page.evaluate(value=>setWorkspace(value,false),workspace);
        const surfaces = await page.evaluate(()=>[document.body,...document.querySelectorAll('.topbar,.workspace-nav,.panel,.panel-head,.summary-tile,.rule-block,input,textarea,select,.mobile-nav')]
          .filter(node=>node.getBoundingClientRect().width && node.getBoundingClientRect().height && getComputedStyle(node).opacity!=='0')
          .map(node=>({name:node.id || node.className || node.tagName,bg:getComputedStyle(node).backgroundColor})));
        if (colorScheme==='dark') for (const surface of surfaces) {
          const channels = surface.bg.match(/[\d.]+/g)?.map(Number) || [];
          if (channels.length===4 && channels[3]===0) continue;
          assert.ok(Math.max(...channels.slice(0,3))<150,`light surface in dark mode: ${surface.name}: ${surface.bg}`);
        }
        assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth-innerWidth)<=1,`${workspace} overflows ${width}`);
        await page.screenshot({path:`/tmp/chr-theme-${colorScheme}-${workspace}-${width}.png`,fullPage:true});
      }
    }
  }
  // The tab strip is a card header: it must stay pinned under the top bar
  // while the page scrolls, at whatever height that bar happens to be.
  for (const width of [1280,390]) {
    await page.setViewportSize({width,height:420});
    await page.evaluate(()=>setWorkspace('history',false));
    await page.evaluate(()=>window.scrollTo(0,500));
    const stack = await page.evaluate(()=>({
      scrolled: window.scrollY,
      barTop: Math.round(document.querySelector('.topbar').getBoundingClientRect().top),
      barBottom: Math.round(document.querySelector('.topbar').getBoundingClientRect().bottom),
      navTop: Math.round(document.querySelector('.workspace-nav').getBoundingClientRect().top),
    }));
    assert.ok(stack.scrolled>0,`page should scroll at ${width}`);
    assert.equal(stack.barTop,0,`top bar stays pinned at ${width}`);
    assert.equal(stack.navTop,stack.barBottom,`tab strip stays under the bar at ${width}`);
    await page.evaluate(()=>window.scrollTo(0,0));
  }
  await page.setViewportSize({width:1280,height:900});
  await page.evaluate(()=>setWorkspace('history',false));
  const oldHistory=historyReads, oldPool=poolReads;
  await page.locator('#historyRefresh').click();
  await page.waitForFunction(()=>!document.querySelector('#historyRefresh').disabled);
  assert.ok(historyReads>oldHistory,'refresh should reload the request history');
  assert.ok(poolReads>oldPool,'refresh should reload the current credential state');
  assert.deepEqual(errors,[]);
  console.log('theme and history refresh: passed');
} finally { await browser.close(); }
