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

  // A directly loaded server sheet, closed on a narrow screen, shows the list.
  await page.setViewportSize({ width: 800, height: 900 });
  await page.goto(origin + '/orgs/' + envPath.split('/')[2] + '/db-servers');
  const server = await page.locator('a.server').first().getAttribute('href');
  await page.goto(origin + server);
  await page.locator('.sheet [data-sheet-close]').click();
  await page.waitForFunction(() => !document.querySelector('.sheet'));
  assert(!(await page.evaluate(() => document.body.classList.contains('has-sheet'))), 'has-sheet stayed on the body');
  assert(await page.locator('a.server').first().isVisible(), 'the server list stayed hidden after closing the sheet');

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
  return 'PASS: dialog after cancel, confirm across refresh, polling, canvas after Back, sheet close on a narrow screen, theme follows the system';
}
