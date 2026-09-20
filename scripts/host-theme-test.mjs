// The panel runs inside the host's iframe: it must follow the host's theme,
// not the operating system's.
import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import('playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--no-sandbox']});
const panel = fs.readFileSync(new URL('../web/index.html', import.meta.url),'utf8');

async function surfaces(page, frame) {
  return page.evaluate(() => {
    const doc = document.querySelector('iframe').contentDocument;
    const bg = getComputedStyle(doc.body).backgroundColor;
    const channels = bg.match(/[\d.]+/g).map(Number);
    return {bg, dark: Math.max(...channels.slice(0,3)) < 120,
            attr: doc.documentElement.getAttribute('data-theme')};
  });
}

for (const os of ['dark','light']) {
  for (const host of ['white','dark',null]) {
    const page = await browser.newPage({viewport:{width:390,height:700},colorScheme:os});
    await page.route('http://host.test/**', async route => {
      const p = new URL(route.request().url()).pathname;
      if (p.endsWith('/panel')) return route.fulfill({contentType:'text/html',body:panel});
      if (p.endsWith('/host')) return route.fulfill({contentType:'text/html',
        body:`<!doctype html><html${host?` data-theme="${host}"`:''}><body style="margin:0">
              <iframe src="http://host.test/panel" style="width:100%;height:680px;border:0"></iframe></body></html>`});
      let data={};
      if (p.endsWith('/credentials')) data={plugin_version:'0',credentials:[{auth_index:'a',name:'a.json',plan_type:'team'}]};
      if (p.endsWith('/rule')) data={rule:{auth_index:'a',enabled:true,set:{},remove:[]}};
      if (p.endsWith('/history')) data={items:[],total:0};
      if (p.endsWith('/turn-states')) data={turn_states:[]};
      await route.fulfill({contentType:'application/json',body:JSON.stringify(data)});
    });
    await page.addInitScript(()=>localStorage.setItem('managementKey','fixture'));
    await page.goto('http://host.test/host');
    await page.waitForTimeout(900);
    const r = await surfaces(page);
    const expectDark = host === 'dark' || (host === null && os === 'dark');
    assert.equal(r.dark, expectDark,
      `os=${os} host=${host ?? 'auto'} -> expected ${expectDark?'dark':'light'} panel, got ${r.bg}`);
    console.log(`os=${os} host=${host ?? 'auto'} -> ${r.dark?'dark':'light'} (data-theme=${r.attr})`);
    await page.close();
  }
}
console.log('host theme: passed');
await browser.close();
