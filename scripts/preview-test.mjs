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
    assert.equal(await page.locator('#cookiePoolMeta').textContent(), '5 / 7 条可用', label);
    // A cookie a degraded turn cooled down reads as cooling, not as usable.
    assert.ok(cookieRows[3].includes('gw-dfw-5.internal') && cookieRows[3].includes('冷却至') && !cookieRows[3].includes('可用'), `${label}: ${cookieRows[3]}`);
    // The cooldown is a per-credential setting that saves on change.
    assert.equal(await page.locator('#cookieCooldown').inputValue(), '1800', label);
    await page.fill('#cookieCooldown', '600');
    await page.dispatchEvent('#cookieCooldown', 'change');
    await page.waitForFunction(() => document.getElementById('toast').textContent.includes('已保存'));
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
    // An entry can replace the credential-level cookie, after a confirmation.
    await page.locator('#drawerBody [data-cookie-use]').click();
    await page.locator('.ant-modal button[data-act=ok]').click();
    await page.waitForFunction(() => document.getElementById('toast').textContent.includes('已替换凭证级 Cookie'));
    await page.locator('#drawerClose').click();
    // A pasted cookie is pooled by its __oailb routing target...
    await page.locator('#cookiePoolAdd').click();
    await page.locator('.ant-modal-input').fill('__oailb=' + (await page.evaluate(() => { const b = (o) => btoa(JSON.stringify(o)).replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_'); return b({ alg: 'HS256' }) + '.' + b({ host: 'gw-pasted.internal', exp: Math.floor(Date.now() / 1000) + 3600 }) + '.' + b('s'); })) + '; oai-did=x');
    await page.locator('.ant-modal button[data-act=ok]').click();
    await page.waitForFunction(() => document.getElementById('toast').textContent.includes('gw-pasted.internal'));
    await page.waitForFunction(() => document.querySelector('#cookiePoolBody .cookie-pool-row')?.textContent.includes('手动'));
    // ...and one without it is refused with the reason.
    await page.locator('#cookiePoolAdd').click();
    await page.locator('.ant-modal-input').fill('__cf_bm=only');
    await page.locator('.ant-modal button[data-act=ok]').click();
    await page.waitForFunction(() => document.getElementById('toast').textContent.includes('没有 __oailb'));
    // Scrolled past, the history card's tab strip stops at the bottom of the
    // header stack instead of sliding under it; the rows keep scrolling.
    const pinned = await page.evaluate(async () => {
      const body = document.getElementById('historyBody');
      const rows = [...body.children];
      for (let i = 0; i < 8; i++) rows.forEach((r) => body.append(r.cloneNode(true)));
      const tabs = document.querySelector('#historyPanel .detail-tabs');
      window.scrollTo({ top: tabs.getBoundingClientRect().top + scrollY + 300, behavior: 'instant' });
      await new Promise((r) => setTimeout(r, 150));
      const result = { tabs: Math.round(tabs.getBoundingClientRect().top), nav: Math.round(document.querySelector('.workspace-nav').getBoundingClientRect().bottom),
        row: Math.round(body.querySelector('tr').getBoundingClientRect().top) };
      for (const extra of [...body.children].slice(rows.length)) extra.remove();
      window.scrollTo({ top: 0, behavior: 'instant' });
      return result;
    });
    assert.equal(pinned.tabs, pinned.nav, `${label}: the strip stops at the header stack ${JSON.stringify(pinned)}`);
    assert.ok(pinned.row < pinned.tabs, `${label}: the rows keep scrolling beneath it`);
    // A manual probe is confirmed, sent, and lands in the probe history marked
    // as manual; the list switches there by itself.
    await page.evaluate(() => { document.getElementById('probeAdvanced').open = true; });
    await page.locator('#probeRunBtn').click();
    await page.locator('.ant-modal button[data-act=ok]').click();
    await page.waitForFunction(() => document.getElementById('toast').textContent.includes('已发起手动探针'));
    await page.waitForFunction(() => document.querySelector('#historyBody .history-summary .history-origin')?.textContent.includes('手动'), null, { timeout: 8000 });
    await page.evaluate(() => document.querySelector('.detail-tabs [data-history="live"]').click());
    await page.waitForFunction(() => document.querySelector('#historyBody .history-summary .history-origin')?.textContent.trim() === '线上');
    // The history list names the backend each request's session was pinned to.
    const historyHeads = await page.locator('#historyPanel thead th').allTextContents();
    assert.ok(historyHeads.includes('请求路由') && historyHeads.includes('响应路由'), `${label}: ${historyHeads}`);
    const routes = await page.locator('#historyBody td.history-route:not(.history-route-res)').allTextContents();
    const resRoutes = await page.locator('#historyBody td.history-route-res').allTextContents();
    assert.equal(routes.length, 6, `${label}: one request route per row`);
    assert.equal(resRoutes.length, 6, `${label}: one response route per row`);
    assert.ok(routes[0].includes('gw-iad-7.internal'), `${label}: ${routes}`);
    assert.ok(resRoutes[0].includes('gw-ord-2.internal'), `${label}: the response moved the session: ${resRoutes}`);
    // The quota strip's refresh asks the upstream and says so.
    await page.locator('#quotaRefresh').click();
    await page.waitForFunction(() => document.getElementById('toast').textContent.includes('已从上游刷新'));
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
