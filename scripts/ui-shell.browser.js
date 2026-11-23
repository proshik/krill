// Run with Playwright's code runner (a function of `page`) while signed in as
// an organization admin and instance operator, on the Deployments tab of a
// running app, e.g. http://localhost:18090/orgs/1/projects/1/environments/1/apps/2
// on the local dev stand. It checks the page frame's client behaviour: the
// confirm dialog after a cancel, a confirmation across a list refresh, the
// canvas after Back, closing a sheet on a narrow screen and the theme
// following the system. The Stop request is intercepted and never reaches
// the server; nothing is changed.
async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message); };
  const appURL = page.url();
  const origin = new URL(appURL).origin;
  const envPath = new URL(appURL).pathname.replace(/\/apps\/\d+$/, '');
  await page.setViewportSize({ width: 1440, height: 900 });

  // A cancelled confirmation leaves the next one usable.
  const stop = page.locator('button[form="stop-form"]');
  assert(await stop.count() === 1, 'the app must be running and show Stop');
  await stop.click();
  await page.locator('#confirm button[value=cancel]').click();
  assert(!(await page.locator('#confirm').evaluate(d => d.open)), 'cancel did not close the dialog');
  await stop.click();
  assert(await page.locator('#confirm').evaluate(d => d.open), 'the dialog did not open again');
  assert(!(await page.locator('#confirm-ok').isDisabled()), 'the confirm button stayed disabled after a cancel');

  // Confirmed after the list refreshed under the open dialog: Stop is still sent.
  let stopSent = false;
  await page.route('**/stop', route => { stopSent = route.request().method() === 'POST'; return route.abort(); });
  await page.waitForResponse(r => r.url().endsWith('/deployments-list'), { timeout: 10000 });
  await page.waitForTimeout(300);
  await page.locator('#confirm-ok').click();
  await page.waitForTimeout(1000);
  await page.unroute('**/stop');
  assert(stopSent, 'a Stop confirmed across a refresh was not sent');

  // A rollback confirmed after the list replaced its form: exactly one
  // request, for the release that was chosen. Needs a release eligible for
  // rollback in the history (KRILL_SECRET_KEY set, two deploys).
  await page.goto(appURL);
  const chosen = page.locator('.history form[action$="/rollback"]').first();
  assert(await chosen.count() === 1, 'no release in the history can be rolled back');
  const chosenAction = await chosen.getAttribute('action');
  const original = await chosen.elementHandle();
  const rollbacks = [];
  await page.route('**/rollback', route => { rollbacks.push(new URL(route.request().url()).pathname); return route.abort(); });
  await chosen.locator('button[type=submit]').click();
  await page.waitForFunction(el => !el.isConnected, original, { timeout: 10000 });
  await page.locator('#confirm-ok').click();
  await page.waitForTimeout(1000);
  assert(rollbacks.length === 1 && rollbacks[0] === chosenAction, 'the confirmed rollback went to ' + JSON.stringify(rollbacks) + ', not ' + chosenAction);

  // The chosen form is gone for good: nothing is sent, and the operator is told.
  await page.goto(appURL);
  rollbacks.length = 0;
  const gone = page.locator('.history form[action$="/rollback"]').first();
  const goneAction = await gone.getAttribute('action');
  await gone.locator('button[type=submit]').click();
  await page.evaluate(a => {
    document.querySelector('[hx-get$="/deployments-list"]').remove(); // no refresh brings it back
    document.querySelectorAll('form').forEach(f => { if (f.getAttribute('action') === a) f.remove(); });
  }, goneAction);
  await page.locator('#confirm-ok').click();
  await page.waitForTimeout(1000);
  await page.unroute('**/rollback');
  assert(rollbacks.length === 0, 'a confirmation whose form is gone sent ' + JSON.stringify(rollbacks));
  assert(await page.locator('#toasts .toast--warn').count() > 0, 'the operator was not told to choose again');

  // A tab still running another release's assets loads its next page in full.
  await page.goto(appURL);
  await page.evaluate(() => { window.__krillOldTab = true; document.body.setAttribute('hx-headers', '{"X-Krill-Assets":"stale"}'); });
  await page.locator('.bar a[href="' + envPath + '/settings"]').click();
  await page.waitForURL(u => u.pathname.endsWith('/settings'));
  await page.waitForLoadState('load');
  assert(await page.evaluate(() => window.__krillOldTab !== true), 'an old tab swapped the new page into its old assets');

  // The list keeps polling after its first refresh.
  await page.goto(appURL);
  await page.waitForResponse(r => r.url().endsWith('/deployments-list'), { timeout: 10000 });
  await page.waitForResponse(r => r.url().endsWith('/deployments-list'), { timeout: 10000 });

  // Back to the canvas from the history cache: zoom still works.
  await page.goto(origin + envPath);
  await page.locator('.bar a[href="' + envPath + '/settings"]').click();
  await page.waitForURL(u => u.pathname.endsWith('/settings'));
  await page.goBack();
  await page.waitForSelector('.plane');
  const zoom = page.locator('[data-zoom-label]');
  const before = await zoom.textContent();
  await page.locator('[data-zoom="in"]').click();
  assert(await zoom.textContent() !== before, 'the canvas restored by Back does not zoom');

  // The new-app sheet: choosing Dockerfile disables the image-only fields, so
  // a registry picked for an image is not sent along with a Dockerfile app.
  await page.goto(origin + envPath + '?new=app');
  await page.locator('input[name=source_type][value=dockerfile]').check({ force: true });
  assert(await page.locator('#na-reg').isDisabled() && await page.locator('#na-image').isDisabled(), 'image-only fields stay enabled for a Dockerfile app');
  await page.locator('input[name=source_type][value=image]').check({ force: true });
  assert(!(await page.locator('#na-reg').isDisabled()), 'image fields stay disabled after going back to image');

  // A directly loaded server sheet, closed on a narrow screen, shows the list.
  await page.setViewportSize({ width: 800, height: 900 });
  await page.goto(origin + '/orgs/' + envPath.split('/')[2] + '/db-servers');
  const server = await page.locator('a.server').first().getAttribute('href');
  await page.goto(origin + server);
  await page.locator('.sheet [data-sheet-close]').click();
  await page.waitForFunction(() => !document.querySelector('.sheet'));
  assert(!(await page.evaluate(() => document.body.classList.contains('has-sheet'))), 'has-sheet stayed on the body');
  assert(await page.locator('a.server').first().isVisible(), 'the server list stayed hidden after closing the sheet');

  // Monitoring draws a worker whatever its name, "constructor" included (the
  // response is the real one plus such a worker, served by an interception).
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.route('**/monitoring/data*', async route => {
    const real = await route.fetch();
    const d = await real.json();
    const len = (d.x || []).length;
    d.nodes = (d.nodes || []).concat([{ node: 'constructor', cpu_pct: 12, mem_used: 1 << 30, mem_total: 4 << 30, ncpu: 2, containers: 1 }]);
    d.series = (d.series || []).concat([{ node: 'constructor', component: 'krill-1', cpu: new Array(len).fill(5), mem: new Array(len).fill(256) }]);
    await route.fulfill({ response: real, json: d });
  });
  await page.goto(origin + '/orgs/' + envPath.split('/')[2] + '/monitoring');
  await page.waitForSelector('.nodecol[aria-label="constructor"]', { timeout: 10000 });
  assert((await page.locator('.mon-status').textContent()).trim() === '', 'monitoring reported an error for a worker named constructor');
  await page.unroute('**/monitoring/data*');

  // A stored choice cycled back to "system" follows the system's changes.
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.emulateMedia({ colorScheme: 'light' });
  await page.evaluate(() => localStorage.setItem('krill-mode', 'dark'));
  await page.goto(appURL);
  assert(await page.evaluate(() => document.documentElement.dataset.choice) === 'dark', 'the stored choice was not applied');
  await page.locator('#mode').click(); // dark -> light (the system's)
  assert(await page.evaluate(() => document.documentElement.dataset.choice) === 'light', 'the toggle skipped light');
  await page.locator('#mode').click(); // light -> back to the system
  assert(await page.evaluate(() => document.documentElement.dataset.choice) === 'system', 'the toggle did not go back to system');
  assert(await page.evaluate(() => localStorage.getItem('krill-mode')) === null, 'system kept a stored choice');
  await page.emulateMedia({ colorScheme: 'dark' });
  await page.waitForFunction(() => document.documentElement.dataset.mode === 'dark', null, { timeout: 3000 });
  await page.emulateMedia({ colorScheme: 'light' });
  return 'PASS: dialog after cancel, confirm across refresh, rollback across refresh, a vanished form sends nothing, an old tab reloads, polling, canvas after Back, sheet close on a narrow screen, a worker named constructor, Dockerfile disables image fields, theme follows the system';
}
