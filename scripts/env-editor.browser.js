// Run with Playwright's browser_run_code_unsafe filename after signing into the
// opt-in TestEnvEditorBrowser fixture and opening its first application's env tab.
// Uses only disposable fixture data; no npm dependencies are needed by Krill.
async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message); };
  await page.bringToFront();
  const appURL = page.url();
  const original = "# Keep comment=unchanged\n PUBLIC =  ordinary  \nTOKEN=private=one\nOTHER=private-two\n";
  // Input values are properties, not attributes, so resolve row indices from DOM.
  async function keyRow(key) {
    const keys = await page.locator('.env-row .env-key').evaluateAll(es => es.map(e => e.value));
    const index = keys.indexOf(key);
    assert(index >= 0, 'missing row ' + key);
    return page.locator('.env-row').nth(index);
  }
  await page.locator('[data-env-mode=kv]').click();
  assert(await page.locator('#env-textarea').inputValue() === original, 'original text changed on mount');
  const widths = await page.locator('.env-row').evaluateAll(rows => rows.map(r => ({key:r.querySelector('.env-key').getBoundingClientRect().width, value:r.querySelector('.env-val').getBoundingClientRect().width})));
  assert(widths.every(w => w.key === widths[0].key && w.value === widths[0].value), 'variable and secret columns differ');
  assert(await page.locator('#env-variable-rows .env-val').getAttribute('type') === 'text', 'ordinary value hidden');
  assert(await page.locator('#env-secret-rows .env-val').evaluateAll(es => es.every(e => e.type === 'password')), 'secret exposed on mount');
  // History must restore values and classification from the same unsaved state.
  await (await keyRow('TOKEN')).locator('button').last().click();
  await page.locator('[onclick="krillEnvAddRow(true)"]').click();
  const replacement = page.locator('#env-variable-rows .env-row').last();
  await replacement.locator('.env-key').fill('TOKEN');
  await replacement.locator('.env-val').fill('public-history');
  await (await keyRow('PUBLIC')).locator('.env-val').fill('ordinary-history');
  await (await keyRow('OTHER')).locator('.env-val').fill('secret-history');
  const historyText = await page.locator('#env-textarea').inputValue();
  await page.locator('a[href="' + new URL(appURL).pathname + '"]').click();
  await page.waitForFunction(() => !document.getElementById('env-form'));
  await page.goBack();
  await page.waitForSelector('#env-form');
  assert(await (await keyRow('TOKEN')).locator('.env-val').inputValue() === 'public-history', 'history restored the old secret as an ordinary value');
  assert(await (await keyRow('PUBLIC')).locator('.env-val').inputValue() === 'ordinary-history', 'history lost ordinary edit');
  assert(await (await keyRow('OTHER')).locator('.env-val').inputValue() === 'secret-history', 'history lost secret edit');
  assert(await (await keyRow('OTHER')).locator('.env-val').getAttribute('type') === 'password', 'history revealed an unchanged secret classification');
  assert(await page.locator('#env-textarea').inputValue() === historyText, 'history source text differs');
  await page.reload();
  await page.locator('[data-env-mode=kv]').click();
  let token = await keyRow('TOKEN');
  await token.locator('.env-eye').click();
  assert(await token.locator('.env-val').getAttribute('type') === 'text', 'individual reveal failed');
  assert(await (await keyRow('OTHER')).locator('.env-val').getAttribute('type') === 'password', 'revealed another secret');
  await token.locator('.env-eye').click();
  assert(await token.locator('.env-val').getAttribute('type') === 'password', 'individual hide failed');
  await token.locator('.env-eye').click();
  await page.locator('[data-env-mode=raw]').click();
  assert(await page.locator('#env-secrets-raw').isHidden(), 'mode switch revealed raw secrets');
  assert(await page.locator('#env-variables-raw').inputValue() === ' PUBLIC =  ordinary  ', 'ordinary raw text lost whitespace');
  await page.locator('#env-raw-reveal').click();
  assert(await page.locator('#env-secrets-raw').isVisible(), 'raw reveal failed');
  await page.locator('[data-env-mode=kv]').click();
  assert(await page.locator('#env-secret-rows .env-val').evaluateAll(es => es.every(e => e.type === 'password')), 'raw reveal carried into rows');
  assert(await page.locator('#env-textarea').inputValue() === original, 'mode round trip lost comments/whitespace');
  // Moving twice must preserve original text and immediately mask the secret.
  await (await keyRow('PUBLIC')).locator('.env-move').click();
  assert(await (await keyRow('PUBLIC')).locator('.env-val').getAttribute('type') === 'password', 'move to secrets did not mask');
  await (await keyRow('PUBLIC')).locator('.env-move').click();
  await (await keyRow('TOKEN')).locator('.env-move').click();
  assert(await page.locator('#env-textarea').inputValue() === original, 'transfer changed text');
  await Promise.all([page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/env')), page.locator('#env-form button[type=submit]').click()]);
  await page.waitForURL(appURL);
  await page.reload();
  await page.waitForFunction(() => document.querySelector('#env-form')?.dataset.mode === 'kv');
  assert(await page.locator('#env-public-keys').inputValue() === 'PUBLIC\nTOKEN', 'classification not persisted');
  assert(await page.locator('#env-textarea').inputValue() === original, 'save changed source text');
  // Raw edits affect one section only and preserve the other section's values.
  await page.locator('[data-env-mode=raw]').click();
  const publicRaw = page.locator('#env-variables-raw');
  await publicRaw.fill((await publicRaw.inputValue()) + '\nEXTRA=  one=two  ');
  await page.locator('#env-raw-reveal').click();
  const secretRaw = page.locator('#env-secrets-raw');
  await secretRaw.fill((await secretRaw.inputValue()).replace('private-two', 'changed-private'));
  await page.locator('[data-env-mode=kv]').click();
  assert(await (await keyRow('EXTRA')).locator('.env-val').inputValue() === '  one=two  ', 'raw value corrupted');
  assert(await (await keyRow('OTHER')).locator('.env-val').inputValue() === 'changed-private', 'secret raw edit lost');
  assert((await page.locator('#env-textarea').inputValue()).includes('# Keep comment=unchanged'), 'comment lost');
  // Add a secret value before its key, then switch modes: draft values survive.
  await page.locator('[onclick="krillEnvAddRow(false)"]').click();
  const draft = page.locator('#env-secret-rows .env-row').last();
  await draft.locator('.env-val').fill('draft-value');
  await page.locator('[data-env-mode=raw]').click();
  await page.locator('#env-raw-reveal').click();
  const draftRaw = page.locator('#env-secrets-raw');
  await draftRaw.fill((await draftRaw.inputValue()).replace('changed-private', 'changed-again'));
  await page.locator('[data-env-mode=kv]').click();
  assert(await page.locator('#env-secret-rows .env-row').last().locator('.env-val').inputValue() === 'draft-value', 'draft value disappeared on mode switch');
  await page.locator('#env-secret-rows .env-row').last().locator('.env-key').fill('DRAFT');
  // BFCache and app navigation must forget reveals.
  await (await keyRow('OTHER')).locator('.env-eye').click();
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })));
  assert(await (await keyRow('OTHER')).locator('.env-val').getAttribute('type') === 'password', 'BFCache kept reveal');
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'mobile horizontal overflow');
  // Exercise HTMX's HTML history cache, which does not preserve JS properties.
  await (await keyRow('OTHER')).locator('.env-eye').click();
  await page.locator('a[href="' + new URL(appURL).pathname + '"]').click();
  await page.waitForFunction(() => !document.getElementById('env-form'));
  await page.goBack();
  await page.waitForSelector('#env-form');
  assert(await page.locator('#env-secret-rows .env-val').evaluateAll(es => es.every(e => e.type === 'password')), 'HTMX history kept reveal');
  await page.goto(appURL.replace('/apps/1?', '/apps/2?'));
  await page.locator('[data-env-mode=kv]').click();
  assert(await page.locator('#env-variable-rows .env-row').count() === 0, 'legacy key became ordinary');
  assert(await page.locator('#env-secret-rows .env-val').getAttribute('type') === 'password', 'next app revealed');
  await page.goto(appURL);
  await page.locator('[data-env-mode=kv]').click();
  assert(await page.locator('#env-secret-rows .env-val').getAttribute('type') === 'password', 'navigation kept reveal');
  // Keyboard transfer preserves the row and focuses its replacement action.
  const other = await keyRow('OTHER');
  await other.locator('.env-move').focus();
  await page.keyboard.press('Enter');
  assert(await (await keyRow('OTHER')).locator('.env-val').getAttribute('type') === 'text', 'keyboard transfer failed');
  await page.keyboard.press('Enter');
  assert(await (await keyRow('OTHER')).locator('.env-val').getAttribute('type') === 'password', 'keyboard return failed');
  await page.locator('[data-env-mode=raw]').click();
  await page.locator('#env-variables-raw').fill(' PUBLIC =  ordinary  \nTOKEN=private=one\nEMPTY=\nRAW=  a=b  ');
  await page.locator('#env-raw-reveal').click();
  await page.locator('#env-secrets-raw').fill('# Keep comment=unchanged\nOTHER=raw-secret\n');
  const savedText = await page.locator('#env-textarea').inputValue();
  await Promise.all([page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/env')), page.locator('#env-form button[type=submit]').click()]);
  await page.waitForURL(appURL);
  await page.reload();
  assert(await page.locator('#env-textarea').inputValue() === savedText, 'raw save changed values/comments');
  assert(await page.locator('#env-secrets-raw').isHidden(), 'save retained raw reveal');
  await page.locator('[data-env-mode=kv]').click();
  assert(await (await keyRow('EMPTY')).locator('.env-val').inputValue() === '', 'empty value lost');
  assert(await (await keyRow('RAW')).locator('.env-val').inputValue() === '  a=b  ', 'raw save lost whitespace');
  await (await keyRow('RAW')).locator('button').last().click();
  assert(!(await page.locator('#env-public-keys').inputValue()).split('\n').includes('RAW'), 'delete retained classification');
  await page.locator('[data-env-mode=raw]').click();
  await page.locator('#env-variables-raw').fill('');
  await page.locator('#env-raw-reveal').click();
  await page.locator('#env-secrets-raw').fill('');
  await Promise.all([page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/env')), page.locator('#env-form button[type=submit]').click()]);
  await page.waitForURL(appURL);
  await page.reload();
  assert(await page.locator('#env-textarea').inputValue() === '', 'empty editor did not persist');
  assert(await page.locator('#env-public-keys').inputValue() === '', 'empty editor retained classification');
  return 'PASS: persistence, transfer, per-row reveal/hide, modes, raw edits, whitespace/comments, drafts, BFCache, HTMX history, app navigation, mobile, equal columns, keyboard, raw save, empty values, deletion';
}
