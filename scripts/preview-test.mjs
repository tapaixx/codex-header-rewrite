// preview.html has to work when opened straight from the filesystem, with no
// CPA and no storage to lean on. Both paths are checked: the storage shim, and
// the takeover that covers browsers which refuse to let a page replace
// window.localStorage.
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const previewURL = new URL('../preview.html', import.meta.url).href;
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});

// Refuse to let the page install a storage shim, and throw on access, the way
// a browser that blocks storage for a file:// origin would.
const blockStorage = `
  Object.defineProperty = ((real) => function (target, prop) {
    if (target === window && (prop === 'localStorage' || prop === 'sessionStorage')) {
      throw new TypeError('cannot redefine ' + prop);
    }
    return real.apply(this, arguments);
  })(Object.defineProperty);
`;

// Reject resolving a path against a file: base, which is what killed the
// preview on the first request in a real browser.
const strictURL = `
  const RealURL = URL;
  window.URL = function (value, base) {
    if (base !== undefined && String(base).startsWith('file:')) {
      throw new TypeError("Failed to construct 'URL': Invalid URL");
    }
    return new RealURL(...arguments);
  };
  window.URL.prototype = RealURL.prototype;
  window.URL.createObjectURL = RealURL.createObjectURL?.bind(RealURL);
  window.URL.revokeObjectURL = RealURL.revokeObjectURL?.bind(RealURL);
`;

try {
  for (const [label, init] of [
    ['storage available', null],
    ['storage blocked', blockStorage],
    ['file: base rejected by URL', strictURL],
  ]) {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(e.message));
    if (init) await page.addInitScript(init);
    await page.setViewportSize({width:1340,height:980});
    await page.goto(previewURL);
    await page.waitForFunction(() => document.body.innerText.includes('alex@example.com'), null, {timeout:8000});

    const banner = await page.evaluate(() => document.getElementById('previewError')?.textContent || null);
    assert.equal(banner, null, `${label}: the preview reported an error: ${banner}`);
    assert.equal(await page.evaluate(() => getComputedStyle(document.getElementById('keyOverlay')).display), 'none',
      `${label}: the key dialog is open, so the preview is asking for a key`);
    assert.equal(await page.locator('#keyChipText').textContent(), '管理密钥：已连接', label);

    // The fixtures reach every workspace, not just the first one.
    await page.evaluate(()=>setWorkspace('history',false));
    await page.waitForSelector('.history-summary');
    assert.equal(await page.locator('.history-summary').count(), 5, label);
    assert.equal(await page.locator('.pool-row').count(), 2, label);

    // And the detail opens with its bodies.
    await page.locator('.history-summary').first().click();
    await page.waitForSelector('.detail-tabs');
    await page.waitForFunction(()=>!document.querySelector('#detailRequestBody').textContent.includes('读取中'));
    await page.locator('.detail-tab[data-pane="reqb"]').click();
    assert.ok((await page.locator('#detailPane-reqb .body-view').textContent()).includes('gpt-6-astra'), label);
    assert.ok((await page.locator('#detailPane-reqb .body-view .jk').count()) > 0, `${label}: bodies are coloured`);

    assert.deepEqual(errors, [], `${label}: page errors`);
    await page.close();
    console.log(`preview (${label}): ok`);
  }
  console.log('preview: passed');
} finally { await browser.close(); }
