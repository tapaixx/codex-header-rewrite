// The panel runs inside the host's iframe: it must follow the host's theme,
// not the operating system's.
import fs from 'node:fs';
import assert from 'node:assert/strict';
const { chromium } = await import('playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--no-sandbox']});
const panel = fs.readFileSync(new URL('../web/index.html', import.meta.url),'utf8');

// Every painted surface, not just the body: a mismatch shows up as some
// panels following the host and others following the system.
async function surfaces(page, workspace) {
  return page.evaluate((ws) => {
    const doc = document.querySelector('iframe').contentDocument;
    const win = doc.defaultView;
    if (ws) win.setWorkspace(ws, false);
    // Chrome reports a color-mix() result as color(srgb r g b) on 0..1 and
    // everything else as rgb() on 0..255; comparing them needs one scale.
    const lum = (colour) => {
      const srgb = colour.startsWith('color(');
      const c = colour.match(/[\d.]+/g).map(Number);
      if (!c.length) return null;
      const alpha = c.length === 4 ? c[3] : 1;
      if (alpha === 0) return null;                     // transparent inherits
      const scale = srgb ? 255 : 1;
      return Math.max(...c.slice(0, 3)) * scale;
    };
    const nodes = [doc.body, ...doc.querySelectorAll(
      '.topbar,.workspace-nav,.workspace-tab,.panel,.panel-head,.panel-body,.summary-tile,' +
      '.rule-block,.chips,.chip-tag,input,select,textarea,.history-table th,.drawer')];
    // The toast and the hint popover are deliberately dark in either theme --
    // they are overlays, not surfaces of the page.
    const seen = [];
    for (const node of nodes) {
      const box = node.getBoundingClientRect();
      if (!box.width || !box.height) continue;
      const style = getComputedStyle(node);
      // A control drawn by its label -- the switch checkbox -- is present but
      // invisible; its own background is never painted.
      if (style.opacity === '0' || style.visibility === 'hidden') continue;
      const value = lum(style.backgroundColor);
      if (value === null) continue;
      seen.push({name: node.id || String(node.className).split(' ')[0] || node.tagName, value});
    }
    return {attr: doc.documentElement.getAttribute('data-theme'),
            bodyDark: lum(getComputedStyle(doc.body).backgroundColor) < 120,
            surfaces: seen};
  }, workspace);
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
    const expectDark = host === 'dark' || (host === null && os === 'dark');
    let checked = 0;
    for (const ws of ['rules','history','decode']) {
      const r = await surfaces(page, ws);
      assert.equal(r.bodyDark, expectDark,
        `os=${os} host=${host ?? 'auto'} ${ws}: body should be ${expectDark?'dark':'light'}`);
      for (const s of r.surfaces) {
        const dark = s.value < 120;
        assert.equal(dark, expectDark,
          `os=${os} host=${host ?? 'auto'} ${ws}: ${s.name} is ${dark?'dark':'light'} (${s.value}) in a ${expectDark?'dark':'light'} panel`);
      }
      checked += r.surfaces.length;
      assert.ok(r.surfaces.length > 5, `${ws}: too few surfaces measured`);
    }
    console.log(`os=${os} host=${host ?? 'auto'} -> ${expectDark?'dark':'light'}, ${checked} surfaces agree`);
    await page.close();
  }
}
console.log('host theme: passed');
await browser.close();
