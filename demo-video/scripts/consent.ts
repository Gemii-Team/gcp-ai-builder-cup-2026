/**
 * Grants K.I.W.I. the Google Meet and Calendar access the Calendar scene needs (the sign-in alone grants neither).
 * Opens a visible Chrome signed in with the saved session on the app's consent route; you approve on Google's screen,
 * and the grant is stored on the server for your account. Needs auth/state.json from `npm run login`.
 */
import fs from 'node:fs';
import path from 'node:path';
import { chromium, type LaunchOptions } from 'playwright';

const BASE = process.env['KIWI_BASE_URL'] ?? 'http://localhost:8080';
const AUTH = path.resolve('auth', 'state.json');

async function main(): Promise<void> {
  if (!fs.existsSync(AUTH)) {
    throw new Error(`no saved session at ${AUTH}: run "npm run login" first`);
  }
  const launch: LaunchOptions = { headless: false, ignoreDefaultArgs: ['--enable-automation'], args: ['--disable-blink-features=AutomationControlled'] };
  const browser = await chromium.launch({ ...launch, channel: 'chrome' }).catch(() => chromium.launch(launch));
  const context = await browser.newContext({ baseURL: BASE, viewport: { width: 1280, height: 820 }, storageState: AUTH });
  const page = await context.newPage();
  await page.goto('/api/auth/meet-consent?return_to=%2Fcalendar');
  console.log('A browser window is open on Google\'s consent screen.');
  console.log('Allow K.I.W.I. to create Meet spaces and calendar events; the app is in testing, so Google shows an "unverified app" notice first.');
  console.log('Waiting up to 10 minutes to come back to the app…');
  await page.waitForURL((url) => url.origin === BASE && !url.pathname.startsWith('/api/'), { timeout: 10 * 60_000 });
  await page.getByRole('navigation', { name: 'Views, channels and agents' }).waitFor({ timeout: 60_000 });
  const missing = new URL(page.url()).searchParams.get('meet_consent') === 'missing';
  console.log(missing ? 'Back in the app, but no grant was stored (the consent was declined or incomplete).' : 'Back in the app: the grant is stored for your account.');
  // The consent round trip signs the browser in afresh, which replaces the session the recorder had: keep the new one.
  const state = await context.storageState();
  const host = new URL(BASE).hostname;
  state.cookies = state.cookies.filter((cookie) => cookie.domain === host);
  fs.writeFileSync(AUTH, JSON.stringify(state));
  console.log(`Session saved to ${AUTH}.`);
  await browser.close();
  process.exit(missing ? 1 : 0);
}

main().catch((error: unknown) => {
  console.error(error);
  process.exit(1);
});
