import { expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import path from 'node:path';

/**
 * The mock provider's accounts (`backend/tools/mockoidc`, `fixtures/users.json`): alice's token carries `hd`, bob's does not
 * and he is seeded (he signs in through the invite rule), carol's does not and nobody seeded her.
 */
export type Who = 'alice' | 'bob' | 'carol';

type Account = { email: string; name: string; hostedDomain: string | null; seeded: boolean };

const accounts = JSON.parse(readFileSync(path.join(import.meta.dirname, 'fixtures', 'users.json'), 'utf8')) as Record<Who, Account>;

export const account = (who: Who): Account => accounts[who];

/** The path of a file the flows upload (`fixtures/files`). */
export const fixtureFile = (name: string): string => path.join(import.meta.dirname, 'fixtures', 'files', name);

/** Signs `who` in the way a person does: the button on `/signin`, then the mock provider's chooser. */
export async function signIn(page: Page, who: Who): Promise<void> {
  await page.goto('/signin');
  await page.getByRole('button', { name: 'Continue with Google' }).click();
  await page.getByRole('button', { name: account(who).email }).click();
}

/** A new context signed in as `who`, on its first page, which has landed in the app. */
export async function signedIn(browser: Browser, who: Who): Promise<{ context: BrowserContext; page: Page }> {
  const context = await browser.newContext();
  const page = await context.newPage();
  await signIn(page, who);
  await expect(page.getByRole('navigation', { name: 'Views, channels and agents' }).or(page.getByRole('navigation', { name: 'Main navigation' }))).toBeVisible();
  return { context, page };
}

/** True at the phone project's width: the rail is hidden and the tab bar takes its place. */
export const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 1440) < 600;

type Ghosts = { __ghosts: number };

/**
 * Counts, from now on, every torn-stub ghost that is put on the page (`kiwi-ghost`, the clone of a confirmed draft's
 * stub that tears away). An observer sees an element that lives for 400ms however it is polled.
 */
export async function watchGhosts(page: Page): Promise<void> {
  await page.evaluate(() => {
    const seen = window as unknown as Ghosts;
    seen.__ghosts = 0;
    new MutationObserver((records) => {
      for (const record of records) {
        for (const node of record.addedNodes) {
          if (node instanceof Element && (node.classList.contains('kiwi-ghost') || node.classList.contains('ghost'))) {
            seen.__ghosts += 1;
          }
        }
      }
    }).observe(document.body, { childList: true, subtree: true });
  });
}

/** How many ghosts `watchGhosts` has seen. */
export const ghostsSeen = (page: Page): Promise<number> => page.evaluate(() => (window as unknown as Ghosts).__ghosts);

/** Attaches a file through the paperclip, the way a person does (the picker is the page's file chooser). */
export async function attachFile(page: Page, name: string): Promise<void> {
  const chooser = page.waitForEvent('filechooser');
  await page.getByRole('button', { name: 'Attach file' }).click();
  await (await chooser).setFiles(fixtureFile(name));
}

/** Types a question into Ask and sends it. */
export async function ask(page: Page, question: string): Promise<void> {
  await page.getByRole('textbox', { name: 'Ask a question, in English or Thai' }).fill(question);
  await page.getByRole('button', { name: 'Ask', exact: true }).click();
}
