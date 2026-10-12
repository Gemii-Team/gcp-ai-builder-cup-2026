import { expect, test } from '@playwright/test';
import { account, fixtureFile, ghostsSeen, isPhone, signIn, signedIn, watchGhosts } from '../helpers';

// The five flows the demo shows, in order, on the seeded workspace: each builds on the data the one before it left.
test.describe.configure({ mode: 'serial' });

const HELLO = 'Hello team · สวัสดีทุกคน';

test('flow 1: sign in and chat', { tag: '@phone' }, async ({ browser }, testInfo) => {
  // Nobody signed in is sent to the sign-in page, and comes back to the page they asked for.
  const anon = await browser.newContext();
  const anonPage = await anon.newPage();
  await anonPage.goto('/c/x');
  await expect(anonPage).toHaveURL(/\/signin\?return_to=%2Fc%2Fx$/);
  await expect(anonPage.getByRole('button', { name: 'Continue with Google' })).toBeVisible();
  await anon.close();

  // alice (a token with `hd`) lands in #general; bob (no `hd`, seeded: the invite rule) joins her there.
  const alice = await signedIn(browser, 'alice');
  const bob = await signedIn(browser, 'bob');
  for (const { page } of [alice, bob]) {
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]{36}$/);
    await expect(page.getByRole('heading', { level: 1 })).toContainText('general');
    const phone = isPhone(page);
    await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible({ visible: phone });
    await expect(page.getByRole('navigation', { name: 'Views, channels and agents' })).toBeVisible({ visible: !phone });
  }

  const log = (page: typeof alice.page) => page.getByRole('log', { name: 'Messages in #general' });
  const row = (page: typeof alice.page, text: string) => log(page).locator('article').filter({ hasText: text });

  // A message reaches the other member without a reload, and its Thai run is marked as Thai.
  await alice.page.getByRole('combobox', { name: 'Message #general' }).fill(HELLO);
  await alice.page.getByRole('button', { name: 'Send' }).click();
  await expect(row(alice.page, HELLO).last()).toBeVisible();
  await expect(row(bob.page, HELLO).last()).toBeVisible({ timeout: 5_000 });
  await expect(row(bob.page, HELLO).last().locator('[lang="th"]')).toHaveText('สวัสดีทุกคน');

  // Offline, a send waits and says so; back online it reaches the other member exactly once.
  const offline = `Offline hello · ส่งตอนออฟไลน์ · ${testInfo.project.name}`;
  await alice.context.setOffline(true);
  await alice.page.getByRole('combobox', { name: 'Message #general' }).fill(offline);
  await alice.page.getByRole('button', { name: 'Send' }).click();
  await expect(row(alice.page, offline).getByRole('status')).toHaveText(/Sending…|Waiting for the connection/);
  await alice.context.setOffline(false);
  await expect(row(alice.page, offline).getByRole('status')).toHaveCount(0, { timeout: 30_000 });
  await expect(row(bob.page, offline)).toHaveCount(1);
  await bob.page.reload();
  await expect(row(bob.page, offline)).toHaveCount(1);

  // A reload keeps the history.
  await alice.page.reload();
  await expect(row(alice.page, HELLO).last()).toBeVisible();
  await expect(row(alice.page, offline)).toHaveCount(1);
  await alice.context.close();
  await bob.context.close();

  // carol has no `hd` and nobody invited her: she is told so, with her address, and is not signed in.
  const carol = await browser.newContext();
  const carolPage = await carol.newPage();
  await signIn(carolPage, 'carol');
  await expect(carolPage).toHaveURL(/\/signin/);
  const notice = carolPage.getByRole('alert');
  await expect(notice).toContainText(account('carol').email);
  await expect(notice).toContainText('has not been invited to this workspace');
  await carolPage.goto('/');
  await expect(carolPage).toHaveURL(/\/signin/);
  await expect(carolPage.getByRole('button', { name: 'Continue with Google' })).toBeVisible();
  await carol.close();
});

test('flow 2: a recording becomes a meeting summary', async ({ browser }) => {
  const { context, page } = await signedIn(browser, 'alice');
  await page.getByRole('link', { name: 'q4-launch' }).click();
  const log = page.getByRole('log', { name: 'Messages in #q4-launch' });
  const drafts = page.getByRole('complementary', { name: 'AI drafts' });
  await expect(drafts).toContainText('3 to confirm');

  // The paperclip takes the recording: a calm wait, then the summary slip with its REF and tracks.
  const attach = async (name: string) => {
    const chooser = page.waitForEvent('filechooser');
    await page.getByRole('button', { name: 'Attach file' }).click();
    await (await chooser).setFiles(fixtureFile(name));
  };
  await attach('sample.wav');
  await expect(log.getByText('Summarizing the recording…')).toBeVisible();
  const slip = log.locator('article').filter({ has: page.getByRole('heading', { level: 2, name: 'sample' }) });
  await expect(slip).toContainText(/REF MTG-\d{4}/);
  await expect(slip.getByRole('listitem').filter({ hasText: /^Decision/ })).toHaveCount(1);
  await expect(slip.getByRole('listitem').filter({ hasText: /^Open question/ })).toHaveCount(1);
  // The replayed ASR has no timing, so no transcript offset is offered (never a false 0:00:00).
  await expect(slip.getByRole('button', { name: /^Transcript at / })).toHaveCount(0);
  await expect(slip).toContainText('2 drafts to confirm');
  await expect(drafts).toContainText('5 to confirm');

  // A recording whose transcript comes back empty fails with its reason and Retry, and issues no REF.
  await attach('empty.wav');
  const failed = log.locator('article').filter({ hasText: "Couldn't summarize empty" });
  await expect(failed).toContainText('no speech was found in the recording');
  await expect(failed.getByRole('button', { name: 'Retry' })).toBeVisible();
  await expect(failed).not.toContainText('REF ');
  await expect(drafts).toContainText('5 to confirm');
  await context.close();
});

test('flow 3: AI drafts become tasks on the Board', { tag: '@phone' }, async ({ browser }, testInfo) => {
  const { context, page } = await signedIn(browser, 'alice');
  const phone = isPhone(page);
  // Flow 2 left two more drafts than the seed has; the desktop run confirms two of the five, the phone run two of the rest.
  const { ownerless, owned } =
    testInfo.project.name === 'phone'
      ? { ownerless: 'Book the weekend crew for the trial', owned: 'Send the pallet count to ops' }
      : { ownerless: 'Draft the delivery promise for the landing page', owned: 'Update the launch checklist with the capacity numbers' };
  await page.goto('/board');

  const band = page.getByRole('complementary', { name: 'AI drafts' });
  const list = band.getByRole('list', { name: 'Drafts to confirm' });
  const draft = (title: string) => list.getByRole('listitem').filter({ has: page.getByRole('heading', { name: title }) });
  await expect(draft(ownerless)).toBeVisible();
  await expect(draft(owned)).toBeVisible();
  const waiting = await list.getByRole('listitem').count();

  // The draft nobody owns cannot be confirmed: Confirm says why, and an owner enables it.
  const confirm = draft(ownerless).getByRole('button', { name: `Confirm: ${ownerless}` });
  await expect(confirm).toBeDisabled();
  await expect(confirm).toHaveAccessibleDescription('Choose an owner first.');
  await draft(ownerless).getByRole('combobox', { name: 'Owner' }).selectOption({ label: 'Alice' });
  await expect(confirm).toBeEnabled();

  // Confirm tears the stub away and seals the slip; what is left is announced; the card lands in To do of its source.
  await watchGhosts(page);
  await confirm.click();
  const todo = page.getByRole('group', { name: 'To do in Q4 launch sync' });
  const card = todo.getByRole('listitem').filter({ hasText: ownerless });
  await expect(card.locator('.torn')).toHaveCount(1);
  await expect(card.locator('.kiwi-perf')).toHaveCount(0);
  const left = waiting - 1;
  await expect(band.getByRole('status')).toHaveText(`Confirmed: ${ownerless}. ${left} ${left === 1 ? 'draft' : 'drafts'} left.`);
  expect(await ghostsSeen(page)).toBeGreaterThan(0);

  // The Move menu works from the keyboard; the moved card is a plain slip.
  await card.getByRole('button', { name: /^Move TSK-\d+: / }).focus();
  await page.keyboard.press('Enter');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  const doing = page.getByRole('group', { name: 'Doing in Q4 launch sync' });
  await expect(doing.getByRole('listitem').filter({ hasText: ownerless })).toBeVisible();
  await expect(doing.getByRole('listitem').filter({ hasText: ownerless }).locator('.torn')).toHaveCount(0);
  await expect(todo.getByRole('listitem').filter({ hasText: ownerless })).toHaveCount(0);

  // With reduced motion nothing is torn away: no ghost ever appears.
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await watchGhosts(page);
  await draft(owned).getByRole('button', { name: `Confirm: ${owned}` }).click();
  await expect(page.getByRole('group', { name: 'To do in Q4 launch sync' }).getByRole('listitem').filter({ hasText: owned })).toBeVisible();
  await expect(todo.getByRole('listitem').filter({ hasText: owned })).toBeVisible();
  expect(await ghostsSeen(page)).toBe(0);
  void phone;
  await context.close();
});
