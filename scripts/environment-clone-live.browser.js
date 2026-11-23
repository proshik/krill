// Drives TestEnvironmentCloneLive (internal/server/environment_clone_live_test.go,
// -tags=integration, KRILL_CLONE_LIVE=1). Run it with Playwright's code runner (a
// function of `page`) after opening the CLONE_LIVE_URL the fixture prints. The
// fixture account and its data exist only in the fixture's disposable database;
// every /_test/ call is the fixture's own check, and /_test/finish cleans up.
async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message); };
  const origin = new URL(page.url()).origin;
  const form = (path, data) => page.request.post(origin + path, { form: data, headers: { Origin: origin } });
  if (new URL(page.url()).pathname === '/login') {
    await page.locator('[name=email]').fill('clone-live@fixture.test');
    await page.locator('[name=password]').fill('pw');
    await Promise.all([page.waitForURL(u => u.pathname !== '/login'), page.locator('form[action="/login"] button[type=submit]').click()]);
  }
  const source = new URL(page.url()).pathname.replace(/\/(settings|databases)$/, '');
  assert(/\/environments\/\d+$/.test(source), 'open CLONE_LIVE_URL, the source environment, first: ' + page.url());

  // Clone from the source environment's Settings page.
  await page.goto(origin + source + '/settings');
  const clone = page.locator('form[action$="/clone"]');
  const name = clone.locator('[name=name]');
  assert(await name.inputValue() === 'staging', 'wrong default name');
  await name.fill('');
  assert(!(await clone.evaluate(f => f.checkValidity())), 'missing required name');
  await name.fill('staging');
  await clone.locator('button[type=submit]').click();
  await page.waitForURL(u => /\/environments\/\d+$/.test(u.pathname) && u.pathname !== source);
  const cloneBase = new URL(page.url()).pathname;

  // The fixture checks the clone and registers its service as owned.
  const checked = await page.request.post(origin + '/_test/check-created');
  assert(checked.ok(), await checked.text());
  const info = await checked.json();
  const env = await form(info.app_url + '/env', {
    env: '# fixture\nPUBLIC=source-public\nTOKEN=staging-secret\nDATABASE_URL=',
    env_public_keys: 'PUBLIC\nDATABASE_URL',
  });
  assert(env.ok(), 'saving the clone\'s variables failed: ' + env.status());

  // Expose the clone's domain.
  await page.goto(origin + info.app_url + '?tab=domains');
  const exposure = page.locator(`form[action$="/domains/${info.domain_id}/exposure"]`);
  await exposure.locator('label:has([name=exposed])').click(); // the switch's track covers the input
  await Promise.all([page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/exposure')), exposure.locator('button[type=submit]').click()]);

  // Deploy it from the Deployments tab: a real build, predeploy and rollout.
  await page.goto(origin + info.app_url);
  await Promise.all([page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/deploy')), page.locator('button[form="deploy-again"]').first().click()]);
  const deadline = Date.now() + 600000;
  for (;;) {
    const status = await (await page.request.get(origin + '/_test/deployment')).json();
    if (status.status === 'done') break;
    if (status.status === 'error' || Date.now() > deadline) throw Error('Real deployment failed: ' + status.error);
    await page.waitForTimeout(1000);
  }
  const headers = { Host: info.host };
  const app = await (await page.request.get(info.gateway + '/', { headers })).json();
  assert(app.public === 'source-public' && app.secret_set && app.metrics_set, 'runtime config mismatch');
  assert(await (await page.request.get(info.gateway + '/count', { headers })).text() === '0', 'clone is not empty');
  const write = await page.request.post(info.gateway + '/write', { headers, form: { value: 'staging-write' } });
  assert(write.status() === 204, 'HTTP write failed');

  // The clone keeps its review notice, also after the router reopens.
  await page.goto(origin + cloneBase);
  await page.reload();
  await page.request.post(origin + '/_test/reopen');
  await page.reload();
  assert(await page.locator('[data-clone-notice]').isVisible(), 'clone notice missing');
  assert(await (await page.request.get(info.gateway + '/count', { headers })).text() === '1', 'write did not persist');
  const finished = await page.request.post(origin + '/_test/finish');
  assert(finished.ok(), await finished.text());
  return await finished.json();
}
