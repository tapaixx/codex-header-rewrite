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
    // The picker shows the email masked; the full address appears nowhere.
    await page.waitForFunction(() => document.body.innerText.includes('a***@example.com'), null, {timeout:8000});
    assert.equal(await page.evaluate(() => document.body.innerText.includes('alex@example.com')), false, 'no unmasked email anywhere');

    const banner = await page.evaluate(() => document.getElementById('previewError')?.textContent || null);
    assert.equal(banner, null, `${label}: the preview reported an error: ${banner}`);
    assert.equal(await page.evaluate(() => getComputedStyle(document.getElementById('keyOverlay')).display), 'none',
      `${label}: the key dialog is open, so the preview is asking for a key`);
    assert.equal(await page.locator('#keyChipText').textContent(), '管理密钥：已连接', label);

    // The fixtures reach every workspace, not just the first one.
    await page.evaluate(()=>setWorkspace('history',false));
    await page.waitForSelector('.history-summary');
    assert.equal(await page.locator('.history-summary').count(), 6, label);
    assert.equal(await page.locator('#turnStateBody .pool-row').count(), 2, label);
    // The credential-level jar opens in the drawer: raw value plus the parsed card.
    await page.locator('#credentialCookieOpen').click();
    await page.waitForFunction(() => document.getElementById('drawerTitle').textContent === '凭证级 Cookie' && !document.getElementById('drawerBody').textContent.includes('读取中'));
    const jar = (await page.locator('#drawerBody').textContent()).replace(/\s+/g,' ');
    assert.ok(jar.includes('路由目标') && jar.includes('gw-iad-7.internal') && jar.includes('原值') && jar.includes('oai-did=demo-device'), `${label}: ${jar.slice(0,200)}`);
    await page.locator('#drawerClose').click();
    // The cookie pool lists one cookie per backend, with its local expiry and
    // a status read from it.
    await page.waitForSelector('#cookiePoolBody .cookie-pool-row');
    const cookieRows = await page.locator('#cookiePoolBody .cookie-pool-row').allTextContents();
    assert.equal(cookieRows.length, 5, `${label}: five to a page`);
    assert.ok(cookieRows[0].includes('gw-iad-7.internal') && cookieRows[0].includes('可用') && /\d\d-\d\d \d\d:\d\d:\d\d/.test(cookieRows[0]), `${label}: ${cookieRows[0]}`);
    assert.ok(cookieRows[1].includes('已过期'), `${label}: ${cookieRows[1]}`);
    assert.equal(await page.locator('#cookiePoolMeta').textContent(), '6 / 7 条可用', label);
    // The pager says how many there are and turns to the rest.
    assert.ok((await page.locator('#cookiePoolPager').textContent()).includes('共 7 条'), label);
    await page.locator('#cookiePoolPager button[aria-label="第 2 页"]').click();
    await page.waitForFunction(() => document.querySelectorAll('#cookiePoolBody .cookie-pool-row').length === 2);
    assert.equal(await page.locator('#cookiePoolPager button[aria-current="true"]').textContent(), '2', label);
    await page.locator('#cookiePoolPager button[aria-label="上一页"]').click();
    await page.waitForFunction(() => document.querySelectorAll('#cookiePoolBody .cookie-pool-row').length === 5);
    await page.locator('#cookiePoolBody .cookie-pool-row').first().click();
    await page.waitForFunction(() => document.getElementById('drawerTitle').textContent === 'Cookie 池');
    assert.ok((await page.locator('#drawerBody').textContent()).includes('多重判断复核通过'), label);
    await page.locator('#drawerClose').click();
    // The history list names the backend each request's session was pinned to.
    const historyHeads = await page.locator('#historyPanel thead th').allTextContents();
    assert.ok(historyHeads.includes('路由目标'), `${label}: ${historyHeads}`);
    const routes = await page.locator('#historyBody td.history-route').allTextContents();
    assert.equal(routes.length, 6, `${label}: one route cell per row`);
    assert.ok(routes[0].includes('gw-iad-7.internal'), `${label}: ${routes}`);
    // The quota strip's refresh asks the upstream and says so.
    await page.locator('#quotaRefresh').click();
    await page.waitForFunction(() => document.getElementById('toast').dataset.show === 'true');
    assert.ok((await page.locator('#toast').textContent()).includes('已从上游刷新'), label);
        // The pool list reads the session's __oailb: routing target and its
    // local issue and expiry times, or says the session has none.
    const heads = await page.locator('.pool-table th').allTextContents();
    assert.ok(heads.includes('路由目标') && heads.includes('签发 (本地)') && heads.includes('过期 (本地)'), `${label}: ${heads}`);
    const firstPool = await page.locator('#turnStateBody .pool-row').first().textContent();
    assert.ok(firstPool.includes('gw-iad-7.internal') && /\d\d-\d\d \d\d:\d\d:\d\d/.test(firstPool), `${label}: ${firstPool}`);
    assert.ok((await page.locator('#turnStateBody .pool-row').nth(1).textContent()).includes('会话里没有 __oailb'), label);
    // Its drawer parses the session instead of showing only the raw string.
    await page.locator('#turnStateBody .pool-row').first().click();
    await page.waitForSelector('#drawerBody .pool-cookie-raw');
    const session = (await page.locator('#drawerBody .history-detail-section').filter({hasText:'会话 Cookie'}).textContent()).replace(/\s+/g,' ');
    assert.ok(session.includes('__oailb') && session.includes('路由目标') && session.includes('gw-iad-7.internal'), `${label}: ${session.slice(0,200)}`);
    assert.equal(await page.locator('#drawerBody .pool-cookie-raw .pool-state-value').isVisible(), false, `${label}: raw value starts collapsed`);
    await page.locator('#drawerClose').click();

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
