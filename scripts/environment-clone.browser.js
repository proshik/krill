// Run with Playwright's code runner after opening CLONE_LIVE_URL printed by
// TestEnvironmentCloneLive. This fixture account exists only in its disposable DB.
async (page) => {
  const origin = new URL(page.url()).origin;
  const source = origin + '/orgs/1/projects/1/environments/1';
  if (new URL(page.url()).pathname === '/login') {
    await page.locator('[name=email]').fill('clone-live@fixture.test');
    await page.locator('[name=password]').fill('pw');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  }
  await page.goto(source);
  await page.getByRole('button', { name: 'Clone environment', exact: true }).click();
  const modal = page.locator('#clone-env');
  const name = modal.locator('[name=name]');
  if (await name.inputValue() !== 'staging') throw Error('Wrong default name');
  await name.fill('');
  if (await modal.locator('form').evaluate(f => f.checkValidity())) throw Error('Missing required name');
  await name.fill('staging');
  await modal.getByRole('button', { name: 'Create', exact: true }).click();
  await page.waitForURL('**/environments/2');
  const checked = await page.request.post(origin + '/_test/check-created');
  if (!checked.ok()) throw Error(await checked.text());
  const info = await checked.json();
  await page.request.post(origin + info.app_url + '/env', { form: {
    env: '# fixture\nPUBLIC=source-public\nTOKEN=staging-secret\nDATABASE_URL=',
    env_public_keys: 'PUBLIC\nDATABASE_URL'
  }});
  await page.goto(origin + info.app_url + '?tab=domains');
  const exposure = page.locator(`form[action$="/domains/${info.domain_id}/exposure"]`);
  await exposure.locator('[name=exposed]').check();
  await exposure.getByRole('button', { name: 'Save', exact: true }).click();
  await page.getByRole('link', { name: 'General', exact: true }).click();
  await page.locator('button[form=deploy-form]').click();
  const deadline = Date.now() + 600000;
  for (;;) {
    const status = await (await page.request.get(origin + '/_test/deployment')).json();
    if (status.status === 'done') break;
    if (status.status === 'error' || Date.now() > deadline) throw Error('Real deployment failed: ' + status.error);
    await page.waitForTimeout(1000);
  }
  const headers = { Host: info.host };
  const app = await (await page.request.get(info.gateway + '/', { headers })).json();
  if (app.public !== 'source-public' || !app.secret_set || !app.metrics_set) throw Error('Runtime config mismatch');
  if (await (await page.request.get(info.gateway + '/count', { headers })).text() !== '0') throw Error('Clone is not empty');
  const write = await page.request.post(info.gateway + '/write', { headers, form: { value: 'staging-write' } });
  if (write.status() !== 204) throw Error('HTTP write failed');
  await page.goto(origin + '/orgs/1/projects/1/environments/2');
  await page.reload();
  await page.request.post(origin + '/_test/reopen');
  await page.reload();
  if (!(await page.locator('main').innerText()).includes('Review')) throw Error('Clone notice missing');
  if (await (await page.request.get(info.gateway + '/count', { headers })).text() !== '1') throw Error('Write did not persist');
  const finished = await page.request.post(origin + '/_test/finish');
  if (!finished.ok()) throw Error(await finished.text());
  return await finished.json();
}
