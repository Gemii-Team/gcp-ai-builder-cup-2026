import { expect, test, type Locator, type Page } from '@playwright/test';
import { account, ask, attachFile, ghostsSeen, isPhone, signIn, signedIn, watchGhosts } from '../helpers';

// The five flows the demo shows, in order, on the seeded workspace: each builds on the data the one before it left.
test.describe.configure({ mode: 'serial' });

// The project name is part of it: the phone run follows the desktop run in the same workspace, and its assertions must not
// be satisfied by the desktop run's message.
const hello = (project: string) => `Hello team · สวัสดีทุกคน ${project}`;

test('flow 1: sign in and chat', { tag: '@phone' }, async ({ browser }, testInfo) => {
  const HELLO_TEXT = hello(testInfo.project.name);
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
  await alice.page.getByRole('combobox', { name: 'Message #general' }).fill(HELLO_TEXT);
  await alice.page.getByRole('button', { name: 'Send' }).click();
  await expect(row(alice.page, HELLO_TEXT).last()).toBeVisible();
  await expect(row(bob.page, HELLO_TEXT).last()).toBeVisible({ timeout: 5_000 });
  await expect(row(bob.page, HELLO_TEXT).last().locator('[lang="th"]')).toHaveText('สวัสดีทุกคน');

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
  await expect(row(alice.page, HELLO_TEXT).last()).toBeVisible();
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
  await attachFile(page, 'sample.wav');
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
  await attachFile(page, 'empty.wav');
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
  // Flow 2 left two more drafts than the seed has, in the lane of the meeting it summarized (`sample`); the desktop run
  // confirms two of the seed's, the phone run two of flow 2's.
  const { ownerless, owned, lane } =
    testInfo.project.name === 'phone'
      ? { ownerless: 'Book the weekend crew for the trial', owned: 'Send the pallet count to ops', lane: 'sample' }
      : { ownerless: 'Draft the delivery promise for the landing page', owned: 'Update the launch checklist with the capacity numbers', lane: 'Q4 launch sync' };
  await page.goto('/board');

  // Below 1024px the drafts are a bottom sheet behind a bar and a lane shows one status at a time; wide, the drafts are a
  // band across the top of the Board and the three statuses sit side by side.
  const dialog = page.getByRole('dialog', { name: 'AI drafts' });
  const band = (phone ? dialog : page).getByRole('complementary', { name: 'AI drafts' });
  const openDrafts = async () => {
    if (phone && !(await dialog.isVisible())) {
      await page.getByRole('button', { name: /drafts to confirm/ }).click();
    }
  };
  const closeDrafts = async () => {
    if (phone) {
      await dialog.getByRole('button', { name: 'Close' }).click();
      await expect(dialog).toBeHidden();
    }
  };
  const showStatus = async (name: 'To do' | 'Doing') => {
    if (phone) {
      await page.getByRole('group', { name: 'Status' }).getByRole('button', { name: new RegExp(`^${name}`) }).click();
    }
  };
  await openDrafts();
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
  const left = waiting - 1;
  await expect(band.getByRole('status')).toHaveText(`Confirmed: ${ownerless}. ${left} ${left === 1 ? 'draft' : 'drafts'} left.`);
  expect(await ghostsSeen(page)).toBeGreaterThan(0);
  if (phone) {
    const sealed = dialog.locator('.torn').filter({ hasText: ownerless });
    await expect(sealed).toHaveCount(1);
    await expect(sealed.locator('.kiwi-perf')).toHaveCount(0);
    await closeDrafts();
  }
  const todo = page.getByRole('group', { name: `To do in ${lane}` });
  const card = todo.getByRole('listitem').filter({ hasText: ownerless });
  await expect(card.locator('.torn')).toHaveCount(1);
  await expect(card.locator('.kiwi-perf')).toHaveCount(0);

  // The Move menu works from the keyboard; the moved card is a plain slip.
  await card.getByRole('button', { name: /^Move TSK-\d+: / }).focus();
  await page.keyboard.press('Enter');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await expect(todo.getByRole('listitem').filter({ hasText: ownerless })).toHaveCount(0);
  await showStatus('Doing');
  const moved = page.getByRole('group', { name: `Doing in ${lane}` }).getByRole('listitem').filter({ hasText: ownerless });
  await expect(moved).toBeVisible();
  await expect(moved.locator('.torn')).toHaveCount(0);

  // With reduced motion nothing is torn away: no ghost ever appears.
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await watchGhosts(page);
  await openDrafts();
  await draft(owned).getByRole('button', { name: `Confirm: ${owned}` }).click();
  await expect(band.getByRole('status')).toContainText(`Confirmed: ${owned}`);
  expect(await ghostsSeen(page)).toBe(0);
  await closeDrafts();
  await showStatus('To do');
  await expect(page.getByRole('group', { name: `To do in ${lane}` }).getByRole('listitem').filter({ hasText: owned })).toBeVisible();
  await context.close();
});

const QUESTION_EN = 'How full is the Bang Na warehouse right now?';
const QUESTION_TH = 'คลังสินค้าบางนาใช้พื้นที่ไปแล้วกี่เปอร์เซ็นต์';
const QUESTION_HR = 'What is the merit increase pool for 2027?';

test('flow 4: cited answers in English and Thai, and the not-found answer', async ({ browser }) => {
  const alice = await signedIn(browser, 'alice');
  await alice.page.goto('/ask');
  const thread = alice.page.getByRole('log', { name: 'Questions and answers' });
  const sources = alice.page.getByRole('complementary', { name: 'Sources' });

  // An English question: a slip with its REF; pressing [1] marks its source row and its slip in the Sources panel.
  await ask(alice.page, QUESTION_EN);
  const answer = thread.locator('.kiwi-slip').filter({ hasText: /REF ANS-\d{4}/ });
  await expect(answer).toHaveCount(1);
  await expect(answer).toContainText('85%');
  const marker = answer.getByRole('button', { name: /^Source 1: / });
  await expect(marker).toHaveAttribute('aria-pressed', 'false');
  await marker.click();
  await expect(marker).toHaveAttribute('aria-pressed', 'true');
  await expect(answer.locator('.kiwi-src[data-hit]')).toHaveCount(1);
  await expect(sources.locator('.ak-sp[data-hit]')).toHaveCount(1);

  // A Thai question: the answer text is marked as Thai.
  await ask(alice.page, QUESTION_TH);
  const thai = thread.locator('.kiwi-slip').filter({ hasText: 'ร้อยละ 85' });
  await expect(thai).toHaveCount(1);
  await expect(thai).toContainText(/REF ANS-\d{4}/);
  await expect(thai.locator('.kiwi-prose [lang="th"]').first()).toContainText('ร้อยละ 85');

  // The same question about #hr-private: alice (a member) gets the answer; bob (not a member) a not-found slip.
  await ask(alice.page, QUESTION_HR);
  await expect(thread.locator('.kiwi-slip').filter({ hasText: /merit increase pool/ })).toContainText('4.2%');
  await alice.context.close();

  const bob = await signedIn(browser, 'bob');
  await bob.page.goto('/ask');
  await ask(bob.page, QUESTION_HR);
  const bobThread = bob.page.getByRole('log', { name: 'Questions and answers' });
  const notFound = bobThread.locator('.kiwi-slip').filter({ hasText: 'Not found in channels you can access' });
  await expect(notFound).toHaveCount(1);
  await expect(notFound).toContainText(/REF ANS-\d{4}/);
  await expect(notFound).not.toContainText('4.2%');
  // A completed answer, not a failure: its perforation is kept and it has no failed styling.
  await expect(notFound.locator('.kiwi-perf')).toHaveCount(1);
  await expect(bobThread.locator('[data-tone="failed"]')).toHaveCount(0);
  // Its proof of scope: the channels searched, and never the private one.
  const searched = notFound.getByRole('list', { name: 'Channels searched' });
  await expect(searched).toContainText('general');
  await expect(searched).toContainText('q4-launch');
  await expect(searched).not.toContainText('hr-private');
  await bob.context.close();
});

test('flow 5: a file becomes a summary, an unsupported one is refused, a scan fails', async ({ browser }) => {
  const { context, page } = await signedIn(browser, 'alice');
  await page.getByRole('link', { name: 'q4-launch' }).click();
  const log = page.getByRole('log', { name: 'Messages in #q4-launch' });
  await expect(log).toContainText('Q4 launch plan');

  // A text file: a calm wait, then its summary slip with a REF.
  await attachFile(page, 'warehouse.txt');
  await expect(log.getByText('Summarizing file…')).toBeVisible();
  const summary = log.locator('article').filter({ hasText: 'cross-dock trial moved' });
  await expect(summary).toContainText(/REF DOC-\d{4}/);
  await expect(log.getByText('Summarizing file…')).toHaveCount(0);

  // A Word document is refused where it is attached, inline, and nothing is left pending. (CreateUpload refuses a type with
  // the same `upload rejected (<reason>)` text as CompleteUpload, so the notice names its reason: this file type isn't supported.)
  await attachFile(page, 'notes.docx');
  await expect(page.getByRole('alert').filter({ hasText: "Couldn't upload notes.docx" })).toHaveCount(1);
  await expect(log.getByText('notes.docx')).toHaveCount(0);
  await expect(log.getByText('Summarizing file…')).toHaveCount(0);

  // A scan with no text layer fails with its reason and Retry, and issues no REF.
  await attachFile(page, 'scan.pdf');
  const failed = log.locator('article').filter({ hasText: "Couldn't summarize scan.pdf" });
  await expect(failed).toContainText('no text could be read from it');
  await expect(failed.getByRole('button', { name: 'Retry' })).toBeVisible();
  await expect(failed).not.toContainText('REF ');
  await context.close();
});

test('flow 3 (continued): a task from a message and a task by hand', { tag: '@phone' }, async ({ browser }, testInfo) => {
  const { context, page } = await signedIn(browser, 'alice');
  const phone = isPhone(page);
  await page.getByRole('link', { name: 'general' }).or(page.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: 'Chat' })).first().click();
  const log = page.getByRole('log', { name: 'Messages in #general' });
  await expect(log).toContainText('Welcome to the K.I.W.I. demo workspace');

  // Make a task sits on a person's message (hover-revealed wide, always there on a phone), never on the assistant's.
  const source = log.locator('article').filter({ hasText: 'Reminder: the Q4 launch review' });
  const make = source.getByRole('button', { name: /^Make a task from Bob's message/ });
  await make.click();
  const title = page.getByRole('textbox', { name: 'Title' });
  await expect(title).toHaveValue(/Q4 launch review/);
  // At 390px the button is icon-only (32px wide; its words are clipped to a pixel, for assistive technology).
  if (phone) {
    expect((await make.boundingBox())?.width ?? Infinity).toBeLessThanOrEqual(34);
  }
  const unique = `Prepare the Q4 launch review ${testInfo.project.name}`;
  await title.fill(unique);
  await page.getByRole('button', { name: 'Create task' }).click();

  // The created card: a plain slip with its REF and a way to the Board, no seal and no perforation, and no owner said neutrally.
  const created = page.getByRole('group', { name: /^Task TSK-\d{4}$/ }).filter({ hasText: unique });
  await expect(created).toContainText(unique);
  await expect(created).toContainText(/REF TSK-\d{4}/);
  await expect(created).toContainText('No owner');
  await expect(created.getByRole('link', { name: 'View on Board' })).toBeVisible();
  await expect(created.locator('.kiwi-seal, .kiwi-perf')).toHaveCount(0);

  // On the Board it sits in To do of the lane of that channel's messages, with its source.
  await page.goto('/board');
  const lane = page.getByRole('region', { name: 'Messages in #general' });
  await expect(lane).toBeVisible();
  const todo = page.getByRole('group', { name: 'To do in Messages in #general' });
  await expect(todo.getByRole('listitem').filter({ hasText: unique })).toContainText('From #general');
  await expect(todo.getByRole('listitem').filter({ hasText: unique })).toContainText('No owner');

  // A task by hand lands in To do of the Added by hand lane.
  await page.getByRole('button', { name: 'New task' }).click();
  const byHand = `Order the pallet labels ${testInfo.project.name}`;
  await page.getByRole('textbox', { name: 'Title' }).fill(byHand);
  await page.getByRole('button', { name: 'Create task' }).click();
  const hand = page.getByRole('group', { name: 'To do in Added by hand' });
  await expect(hand.getByRole('listitem').filter({ hasText: byHand })).toContainText(/REF TSK-\d{4}/);
  await expect(hand.getByRole('listitem').filter({ hasText: byHand })).toContainText('No owner');
  await context.close();
});

// The live run (KIWI_LIVE_E2E=1, overlay mockauth: the real LiteLLM and Ollama) checks structure only: a slip with its REF
// appears, Thai text is marked as Thai. No assertion names a word the model wrote. Only one recording is transcribed.
const LIVE_WAIT = 200_000;

test('flow 2 live: a recording becomes a meeting summary', { tag: '@live' }, async ({ browser }) => {
  test.setTimeout(240_000);
  const { context, page } = await signedIn(browser, 'alice');
  await page.getByRole('link', { name: 'q4-launch' }).click();
  const log = page.getByRole('log', { name: 'Messages in #q4-launch' });
  await attachFile(page, 'sample.wav');
  const slip = log.locator('article').filter({ has: page.getByRole('heading', { level: 2, name: 'sample' }) });
  await expect(slip).toContainText(/REF MTG-\d{4}/, { timeout: LIVE_WAIT });
  await context.close();
});

// The same fact as QUESTION_EN, asked again in other words and then in the other language: the conversation of the Ask panel
// already holds its answer, which is where a real model is tempted to take it from instead of searching.
const FOLLOW_UP_EN = 'In other words, what share of the Bang Na pallet positions is taken today?';
const FOLLOW_UP_TH = 'ตอนนี้คลังสินค้าบางนาเต็มแค่ไหน';
const QUESTION_TH_LIVE = 'วันเปิดตัวตามแผนไตรมาสสี่คือวันที่เท่าไร';

/**
 * Asks a follow-up on a topic the conversation already holds and checks only the structure of what comes back: every Ask is
 * answered from the search of its own turn, so the newest slip has its REF and is either an answer with at least one source
 * row or the not-found slip that says where it looked, and no slip of the thread is a failure. Returns that slip.
 */
async function askFollowUp(page: Page, thread: Locator, slips: Locator, question: string, settled: number): Promise<Locator> {
  const failed = thread.locator('[data-tone="failed"]');
  await ask(page, question);
  await expect(slips.or(failed)).toHaveCount(settled, { timeout: LIVE_WAIT });
  await expect(failed, 'the follow-up ended as a failed slip').toHaveCount(0);
  await expect(thread).not.toContainText('something went wrong');
  const slip = slips.first(); // newest first
  if (await slip.evaluate((el) => el.classList.contains('ak-nf'))) {
    await expect(slip.getByRole('list', { name: 'Channels searched' })).toBeVisible();
  } else {
    await expect(slip.locator('.kiwi-src').first()).toBeVisible();
  }
  return slip;
}

test('flow 4 live: questions get slips with a REF, a Thai one in Thai', { tag: '@live' }, async ({ browser }) => {
  test.setTimeout(420_000);
  const { context, page } = await signedIn(browser, 'alice');
  await page.goto('/ask');
  const thread = page.getByRole('log', { name: 'Questions and answers' });
  const slips = thread.locator('.kiwi-slip').filter({ hasText: /REF ANS-\d{4}/ });
  await ask(page, QUESTION_EN);
  await expect(slips).toHaveCount(1, { timeout: LIVE_WAIT });

  // Follow-ups on the same topic, in English and then in Thai: each is a slip with a REF that cites a source or says nothing was
  // found, never plain text from the conversation and never the failed slip. A Thai answer's text is marked as Thai.
  await askFollowUp(page, thread, slips, FOLLOW_UP_EN, 2);
  const thai = await askFollowUp(page, thread, slips, FOLLOW_UP_TH, 3);
  if (!(await thai.evaluate((el) => el.classList.contains('ak-nf')))) {
    await expect(thai.locator('.kiwi-prose [lang="th"]').first()).toBeVisible();
  }

  // Another topic, in Thai: an answer slip in Thai.
  await ask(page, QUESTION_TH_LIVE);
  await expect(slips).toHaveCount(4, { timeout: LIVE_WAIT });
  await expect(slips.first().locator('.kiwi-prose [lang="th"]').first()).toBeVisible();
  await context.close();
});

test('flow 5 live: a file becomes a summary with a REF', { tag: '@live' }, async ({ browser }) => {
  test.setTimeout(240_000);
  const { context, page } = await signedIn(browser, 'alice');
  await page.getByRole('link', { name: 'q4-launch' }).click();
  const log = page.getByRole('log', { name: 'Messages in #q4-launch' });
  await attachFile(page, 'warehouse.txt');
  const summary = log.locator('article').filter({ hasText: 'warehouse.txt' }).filter({ hasText: /REF DOC-\d{4}/ });
  await expect(summary).toHaveCount(1, { timeout: LIVE_WAIT });
  await context.close();
});
