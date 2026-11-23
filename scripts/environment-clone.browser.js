// Run with Playwright's code runner (a function of `page`) while signed in as
// an organization admin, on the Settings page of a production environment
// with at least one image app and no "staging" environment next to it, e.g.
// http://localhost:18090/orgs/1/projects/2/environments/3/settings on the
// local dev stand. It seeds the first app's variables, clones the
// environment, checks the clone and the untouched source, starts a deploy of
// the clone's app and deletes the clone again. Use a disposable dev stand,
// never a real install.
async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message); };
  const origin = new URL(page.url()).origin;
  const sourceBase = new URL(page.url()).pathname.replace(/\/settings$/, '');
  const post = async (path, form) => {
    const r = await page.request.post(origin + path, { form, headers: { Origin: origin }, maxRedirects: 0 });
    assert(r.status() === 303, path + ' answered ' + r.status());
  };
  const appPath = async () => {
    const href = await page.locator('a.svc[href*="/apps/"]').first().getAttribute('href');
    assert(href, 'no app on the canvas');
    return href;
  };

  // Seed the source app: one ordinary variable, one secret.
  await page.goto(origin + sourceBase);
  const sourceApp = await appPath();
  const sourceEnv = '# fixture\nPUBLIC=source-public\nTOKEN=source-secret\n';
  await post(sourceApp + '/env', { env: sourceEnv, env_public_keys: 'PUBLIC' });

  // The clone form on the source's settings: a default name, and a name is required.
  await page.goto(origin + sourceBase + '/settings');
  const form = page.locator('form[action$="/clone"]');
  const name = form.locator('[name=name]');
  assert(await name.inputValue() === 'staging', 'wrong default name: ' + await name.inputValue());
  await name.fill('');
  assert(!(await form.evaluate(f => f.checkValidity())), 'an empty name was accepted');
  await name.fill('staging');
  await form.locator('button[type=submit]').click();
  await page.waitForURL(u => u.pathname !== sourceBase && /\/environments\/\d+$/.test(u.pathname));
  const cloneBase = new URL(page.url()).pathname;

  // The clone says it needs a review, and keeps saying so after a reload.
  assert(await page.locator('[data-clone-notice]').isVisible(), 'clone notice missing');
  await page.reload();
  assert(await page.locator('[data-clone-notice]').isVisible(), 'clone notice gone after a reload');
  await page.goto(origin + cloneBase + '/settings');
  assert(await page.locator('[data-clone-notice]').isVisible(), 'clone notice missing on its settings');

  // Ordinary values are copied, secret values are cleared.
  await page.goto(origin + cloneBase);
  const cloneApp = await appPath();
  assert(cloneApp !== sourceApp, 'the clone shows the source app');
  await page.goto(origin + cloneApp + '?tab=variables');
  const cloned = await page.locator('#env-textarea').inputValue();
  assert(cloned.includes('PUBLIC=source-public'), 'ordinary value not copied: ' + cloned);
  assert(!cloned.includes('source-secret'), 'a secret value was copied');

  // Editing the clone leaves the source alone.
  await post(cloneApp + '/env', { env: 'PUBLIC=staging-public\nTOKEN=staging-secret\n', env_public_keys: 'PUBLIC' });
  await page.goto(origin + sourceApp + '?tab=variables');
  assert(await page.locator('#env-textarea').inputValue() === sourceEnv, 'the source changed with its clone');

  // Exposing the clone's domain sticks.
  await page.goto(origin + cloneApp + '?tab=domains');
  const exposure = page.locator('form[action$="/exposure"]').first();
  await exposure.locator('label:has([name=exposed])').click(); // the switch's track covers the input
  assert(await exposure.locator('[name=exposed]').isChecked(), 'the exposure switch did not turn on');
  await Promise.all([page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/exposure')), exposure.locator('button[type=submit]').click()]);
  await page.reload();
  assert(await page.locator('form[action$="/exposure"]').first().locator('[name=exposed]').isChecked(), 'exposure not saved');

  // Starting the clone's app queues a deployment.
  await page.goto(origin + cloneApp);
  const start = page.locator('button[form="deploy-again"]').first();
  await Promise.all([page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/deploy')), start.click()]);
  await page.goto(origin + cloneApp);
  await page.waitForSelector('#deployments-list a[href*="/deployments/"]');

  // Delete the clone through its settings, with the confirmation dialog.
  await page.goto(origin + cloneBase + '/settings');
  await page.locator('form[action="' + cloneBase + '/delete"] button[type=submit]').click();
  await page.locator('#confirm-ok').click();
  await page.waitForURL(u => !u.pathname.startsWith(cloneBase));
  const gone = await page.request.get(origin + cloneBase, { maxRedirects: 0 });
  assert(gone.status() === 404, 'the clone was not deleted: ' + gone.status());
  return 'PASS: clone form, notice after reload, ordinary copied and secrets cleared, source isolation, exposure saved, deploy queued, clone deleted';
}
