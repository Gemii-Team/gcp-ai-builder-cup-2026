/**
 * One-time sign-in for the recorder. Opens a visible Chromium on the sign-in page; you sign in with your Google account
 * yourself (the recorder never sees a password), and once the workspace has loaded the session is saved to auth/state.json,
 * which `npm run record` reuses. The file is git-ignored. Run it again whenever the session has expired.
 */
import fs from 'node:fs';
import path from 'node:path';
import { chromium, type LaunchOptions } from 'playwright';

const BASE = process.env['KIWI_BASE_URL'] ?? 'http://localhost:8080';
const AUTH = path.resolve('auth', 'state.json');

async function main(): Promise<void> {
  fs.mkdirSync(path.dirname(AUTH), { recursive: true });
  // Google refuses to sign in a browser that announces automation ("This browser or app may not be secure"), so the
  // window is the machine's own Chrome when there is one, without Playwright's automation flag. Only this sign-in uses
  // it; the recorder itself never leaves localhost.
  const launch: LaunchOptions = {
    headless: false,
    ignoreDefaultArgs: ['--enable-automation'],
    args: ['--disable-blink-features=AutomationControlled'],
  };
  const browser = await chromium.launch({ ...launch, channel: 'chrome' }).catch(() => chromium.launch(launch));
  const context = await browser.newContext({ baseURL: BASE, viewport: { width: 1280, height: 820 } });
  const page = await context.newPage();
  await page.goto('/signin');
  console.log(`A browser window is open at ${BASE}/signin.`);
  console.log('Press "Continue with Google" there and sign in with the account that is seeded in K.I.W.I.');
  console.log('Waiting up to 10 minutes for the workspace to load…');
  await page.getByRole('navigation', { name: 'Views, channels and agents' }).waitFor({ timeout: 10 * 60_000 });
  // Only the app's own session is kept: Google's account cookies have no business in a file on disk.
  const state = await context.storageState();
  const host = new URL(BASE).hostname;
  state.cookies = state.cookies.filter((cookie) => cookie.domain === host);
  fs.writeFileSync(AUTH, JSON.stringify(state));
  console.log(`Signed in. Session saved to ${AUTH}. You can close the window; run "npm run record" next.`);
  await browser.close();
}

main().catch((error: unknown) => {
  console.error(error);
  process.exit(1);
});
