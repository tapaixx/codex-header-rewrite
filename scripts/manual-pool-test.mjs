// Under the paused judgement a live request's state waits for the operator:
// the row says so, the analysis tab offers the action, and pooling it updates
// the row, the drawer and the pool list without reopening anything.
import assert from 'node:assert/strict';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', e => errors.push(e.message));
await page.addInitScript(() => {
  window.__copies = 0;
  Object.defineProperty(navigator, 'clipboard', {value: {writeText: () => { window.__copies++; return Promise.resolve(); }}});
});
try {
  await page.setViewportSize({width:1440,height:1000});
  await page.goto(new URL('../preview.html', import.meta.url).href);
  await page.waitForSelector('#rulePanel');
  await page.evaluate(() => setWorkspace('history', false));
  await page.waitForSelector('#historyBody .history-summary');
  await page.waitForSelector('#turnStateBody .pool-row');

  const firstRow = page.locator('#historyBody .history-summary').first();
  assert.ok((await firstRow.textContent()).includes('需手动入池'), 'the live row says it waits for a manual pool');
  const retryRow = page.locator('#historyBody .history-summary').filter({hasText:'重试 2 次'});
  assert.ok((await retryRow.textContent()).includes('已入池'), 'the plugin\'s own retry still pools itself');

  await firstRow.click();
  await page.waitForSelector('#drawerBody .detail-tabs');
  const action = page.locator('#detailPane-analysis [data-manual-pool]');
  assert.equal(await action.count(), 1, 'the upstream card offers the action');
  assert.equal(await action.isEnabled(), true);
  const inTitle = await action.evaluate((el) => el.closest('.ts-block')?.querySelector('.ts-title')?.textContent.includes('上游返回'));
  assert.ok(inTitle, 'the action sits on the upstream card, not the outbound one');

  // The same drawer pools the request's session into the Cookie 池.
  await page.locator('#detailPane-analysis [data-cookie-pool]').click();
  await page.waitForFunction(() => document.getElementById('toast').textContent.includes('已入池：路由目标 gw-iad-7.internal'));
  await page.waitForFunction(() => document.querySelector('#cookiePoolBody .cookie-pool-row')?.textContent.includes('手动'));
  const before = await page.evaluate(() => store.turnStates.map((s) => `${s.model}:${s.digest}`));
  assert.ok(before.includes('gpt-6-astra:c91a0e77bb42'), JSON.stringify(before));
  await action.click();
  await page.waitForFunction(() => document.getElementById('toast').textContent.includes('已手动入池'));
  const message = await page.locator('#toast').textContent();
  assert.ok(message.includes('已手动入池') && message.includes('替换'), 'the toast says it took the slot from a newer state: ' + message);
  await page.waitForFunction(() => store.turnStates.some((s) => s.digest === 'a13f9c21b4e0'));

  assert.equal(await page.locator('#detailPane-analysis [data-manual-pool]').count(), 0, 'the action is gone once pooled');
  assert.equal(await page.locator('#historyDrawer').isVisible(), true, 'the drawer stays open');
  const drawerTags = await page.locator('#drawerBody .history-detail-label').first().textContent();
  assert.ok(drawerTags.includes('手动入池') && !drawerTags.includes('需手动入池'), drawerTags);
  const rowText = await page.locator('#historyBody .history-summary').first().textContent();
  assert.ok(rowText.includes('手动入池') && !rowText.includes('需手动入池'), rowText);

  // Probe rows pool themselves and never get the action.
  await page.locator('#drawerClose').click();
  await page.evaluate(() => { store.historyTab = 'probe'; return loadHistory(1); });
  await page.waitForSelector('#historyBody .history-summary');
  await page.locator('#historyBody .history-summary').first().click();
  await page.waitForSelector('#drawerBody .detail-tabs');
  assert.equal(await page.locator('#drawerBody [data-manual-pool]').count(), 0, 'probe rows have no manual pool');

  // The drawer body outlives the records shown in it. Its click handlers are
  // wired once, so after several records one click still copies once.
  await page.locator('#drawerClose').click();
  await page.evaluate(() => { store.historyTab = 'live'; return loadHistory(1); });
  await page.waitForFunction(() => document.querySelector('#historyBody .history-summary')?.textContent.includes('手动入池') && !document.querySelector('#historyBody .history-summary')?.textContent.includes('需手动入池'));
  for (const index of [1, 2, 0]) {
    await page.evaluate((i) => document.querySelectorAll('#historyBody .history-summary')[i].click(), index);
    await page.waitForSelector('#drawerBody .detail-tabs');
  }
  await page.locator('#drawerBody .detail-tab[data-pane="reqb"]').click();
  await page.locator('#detailPane-reqb [data-body-act="copy"]').click();
  await page.waitForTimeout(150);
  assert.equal(await page.evaluate(() => window.__copies), 1, 'one click, one copy');

  assert.deepEqual(errors, []);
  console.log('manual pool: passed');
} finally {
  await browser.close();
}
