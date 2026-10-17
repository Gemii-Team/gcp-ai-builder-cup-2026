/**
 * Records the demo walkthrough of the running K.I.W.I. app (http://localhost:8080 unless KIWI_BASE_URL says otherwise).
 *
 * Playwright drives a headless Chromium through the shot list of the demo script (sign-in page, the workspace rail,
 * a channel, the Q4 launch sync meeting and its draft tasks, the Board, Ask, the Calendar), while a CDP screencast
 * captures every change of the screen as a 1920x1080 JPEG in public/frames/. The pointer's path and the scene marks are
 * timed against the same clock and written with the frame list to public/scenes.json, which the Remotion composition
 * plays back. The frames carry no pointer of their own: the composition draws it from the path.
 *
 * It needs the session that `npm run login` saved (auth/state.json). Flags:
 *   --probe        record only the sign-in page for a few seconds (no session needed) to check the pipeline
 *   --check        only ask the warm-up question and report whether the model answered (nothing is recorded)
 *   --no-schedule  skip the Schedule meeting form altogether
 *   --real-google  send Schedule meeting to the real server (needs the Calendar grant; creates a real Google Calendar event).
 *                  Without it the recorder answers ScheduleMeeting and ListUpcoming itself with a simulated event, so the
 *                  scene needs no Google consent and leaves nothing on any calendar.
 *   --no-warmup    skip the unrecorded Ask question that warms the model up before the recorded one
 *   --thai         also ask a question in Thai after the English one
 *   --headed       show the browser while it records
 */
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import path from 'node:path';
import { chromium, type Browser, type BrowserContext, type Locator, type Page } from 'playwright';

const BASE = process.env['KIWI_BASE_URL'] ?? 'http://localhost:8080';
const AUTH = path.resolve('auth', 'state.json');
const FRAMES = path.resolve('public', 'frames');
const MANIFEST = path.resolve('public', 'scenes.json');
/**
 * The screencast always comes out at the viewport's CSS size, so the viewport is the output size. It is rendered at 2x and
 * scaled down by Chrome, which keeps the text crisp, and the page is zoomed a little (ZOOM) so the interface reads well on video.
 */
const VIEWPORT = { width: 1920, height: 1080 };
const OUTPUT = { width: 1920, height: 1080 };
const DPR = 2;
const ZOOM = 1.15;

const flags = new Set(process.argv.slice(2));
const PROBE = flags.has('--probe');
const CHECK = flags.has('--check');
const HEADED = flags.has('--headed');
const SCHEDULE = !flags.has('--no-schedule');
const WARMUP = !flags.has('--no-warmup');
const THAI = flags.has('--thai');
const MOCK_GOOGLE = !flags.has('--real-google');

const QUESTION_EN = 'What did we decide about the warehouse?';
const QUESTION_TH = 'เราตัดสินใจเรื่องโกดังสำรองว่าอย่างไร';
const CHAT_MESSAGE = 'Capacity note is updated, please read it before Thursday · อัปเดตตัวเลขโกดังบางนาแล้วนะคะ';
const OFFLINE_MESSAGE = 'Sending this one while offline…';
const MEETING_TITLE = 'Q4 launch sync';
const EVENT_TITLE = 'Launch rehearsal';

type FrameRec = { t: number; f: string };
type CursorRec = { t: number; x: number; y: number; k: 'm' | 'd' | 'u' };
type SceneRec = { id: string; t0: number; t1: number };
type EventRec = { t: number; k: 'chime' };

const now = (): number => Date.now() / 1000;
const sleep = (seconds: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, seconds * 1000));
const easeInOut = (p: number): number => (p < 0.5 ? 4 * p * p * p : 1 - Math.pow(-2 * p + 2, 3) / 2);
const log = (message: string): void => console.log(`[${new Date().toISOString().slice(11, 19)}] ${message}`);

/** Width and height of a JPEG, from its first frame header. */
function jpegSize(buffer: Buffer): { w: number; h: number } | null {
  let i = 2;
  while (i + 9 < buffer.length) {
    if (buffer[i] !== 0xff) {
      return null;
    }
    const marker = buffer[i + 1]!;
    const length = buffer.readUInt16BE(i + 2);
    if (marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc) {
      return { h: buffer.readUInt16BE(i + 5), w: buffer.readUInt16BE(i + 7) };
    }
    i += 2 + length;
  }
  return null;
}

/** Collects the screencast frames, the pointer's path and the scene marks of every page it is attached to. */
class Recorder {
  readonly frames: FrameRec[] = [];
  readonly cursor: CursorRec[] = [];
  readonly scenes: SceneRec[] = [];
  readonly events: EventRec[] = [];
  private seq = 0;
  private lastKept = -Infinity;
  private readonly writes: Promise<void>[] = [];
  private open: SceneRec | null = null;

  async attach(page: Page): Promise<void> {
    const cdp = await page.context().newCDPSession(page);
    cdp.on('Page.screencastFrame', (event) => {
      // Chrome sends the next frame only once this one is acknowledged.
      void cdp.send('Page.screencastFrameAck', { sessionId: event.sessionId }).catch(() => undefined);
      const t = event.metadata.timestamp ?? now();
      if (t - this.lastKept < 1 / 31) {
        return; // the composition runs at 30 fps; more is wasted disk
      }
      this.lastKept = t;
      const f = `${String(this.seq++).padStart(5, '0')}.jpg`;
      this.frames.push({ t, f });
      this.writes.push(fsp.writeFile(path.join(FRAMES, f), Buffer.from(event.data, 'base64')));
    });
    await cdp.send('Page.startScreencast', { format: 'jpeg', quality: 92, maxWidth: OUTPUT.width, maxHeight: OUTPUT.height, everyNthFrame: 1 });
  }

  scene(id: string): void {
    const t = now();
    if (this.open !== null) {
      this.open.t1 = t;
    }
    this.open = { id, t0: t, t1: t };
    this.scenes.push(this.open);
    log(`scene: ${id}`);
  }

  /** A moment the soundtrack marks (an AI result arriving, a task confirmed). */
  event(k: EventRec['k']): void {
    this.events.push({ t: now(), k });
  }

  mark(k: CursorRec['k'], x: number, y: number): void {
    this.cursor.push({ t: now(), x, y, k });
  }

  async finish(): Promise<void> {
    if (this.open !== null) {
      this.open.t1 = now();
    }
    await Promise.all(this.writes);
    const first = this.frames[0];
    if (first === undefined) {
      throw new Error('no frame was captured');
    }
    const rel = (t: number): number => Math.round((t - first.t) * 1000) / 1000;
    const manifest = {
      recordedAt: new Date().toISOString(),
      viewport: VIEWPORT,
      output: OUTPUT,
      frames: this.frames.map((frame) => ({ t: rel(frame.t), f: frame.f })),
      cursor: this.cursor.map((c) => ({ t: rel(c.t), x: Math.round(c.x * 10) / 10, y: Math.round(c.y * 10) / 10, k: c.k })),
      scenes: this.scenes.map((scene) => ({ id: scene.id, t0: rel(scene.t0), t1: rel(scene.t1) })),
      events: this.events.map((event) => ({ t: rel(event.t), k: event.k })),
    };
    await fsp.writeFile(MANIFEST, JSON.stringify(manifest));
    const size = jpegSize(await fsp.readFile(path.join(FRAMES, first.f)));
    const last = this.frames[this.frames.length - 1]!;
    log(`wrote ${manifest.frames.length} frames (${size === null ? 'size unknown' : `${size.w}x${size.h}`}), ${manifest.cursor.length} pointer samples, ${rel(last.t).toFixed(1)}s of recording`);
    for (const scene of manifest.scenes) {
      log(`  ${scene.id.padEnd(10)} ${scene.t0.toFixed(1).padStart(6)}s → ${scene.t1.toFixed(1).padStart(6)}s  (${(scene.t1 - scene.t0).toFixed(1)}s)`);
    }
    if (size !== null && (size.w !== OUTPUT.width || size.h !== OUTPUT.height)) {
      log(`WARNING: frames are ${size.w}x${size.h}, not ${OUTPUT.width}x${OUTPUT.height}; the composition scales them, but check the result`);
    }
  }
}

/** The pointer: it glides, it presses, it types, the way a hand does, and tells the recorder where it is. */
class Hand {
  x: number;
  y: number;
  constructor(
    private readonly page: Page,
    private readonly rec: Recorder,
    start: { x: number; y: number },
  ) {
    this.x = start.x;
    this.y = start.y;
  }

  async glide(x: number, y: number, seconds: number): Promise<void> {
    const steps = Math.max(10, Math.round(seconds * 60));
    const sx = this.x;
    const sy = this.y;
    for (let i = 1; i <= steps; i++) {
      const p = easeInOut(i / steps);
      this.x = sx + (x - sx) * p;
      this.y = sy + (y - sy) * p;
      await this.page.mouse.move(this.x, this.y);
      this.rec.mark('m', this.x, this.y);
      await sleep(1 / 60);
    }
  }

  /** Scrolls, a notch at a time, until the target's box is inside the viewport (a jump would show in the recording). */
  private async bringIntoView(target: Locator): Promise<void> {
    for (let i = 0; i < 30; i++) {
      const box = await target.boundingBox();
      if (box === null) {
        return;
      }
      if (box.y + box.height > VIEWPORT.height - 24) {
        await this.page.mouse.wheel(0, 120);
      } else if (box.y < 24) {
        await this.page.mouse.wheel(0, -120);
      } else {
        return;
      }
      await sleep(0.06);
    }
  }

  async to(target: Locator, opts: { ax?: number; ay?: number; seconds?: number } = {}): Promise<void> {
    await target.waitFor({ state: 'visible' });
    await this.bringIntoView(target);
    const box = await target.boundingBox();
    if (box === null) {
      throw new Error('the target has no box to move to');
    }
    const x = box.x + box.width * (opts.ax ?? 0.5);
    const y = box.y + box.height * (opts.ay ?? 0.5);
    const seconds = opts.seconds ?? Math.min(1.1, 0.35 + Math.hypot(x - this.x, y - this.y) / 1500);
    await this.glide(x, y, seconds);
  }

  /** A press where the pointer is. `real: false` records the press for the overlay without dispatching it. */
  async press(real = true): Promise<void> {
    this.rec.mark('d', this.x, this.y);
    if (real) {
      await this.page.mouse.down();
    }
    await sleep(0.09);
    if (real) {
      await this.page.mouse.up();
    }
    this.rec.mark('u', this.x, this.y);
  }

  async click(target: Locator, opts: { ax?: number; ay?: number; seconds?: number } = {}): Promise<void> {
    await this.to(target, opts);
    await sleep(0.18);
    await this.press();
  }

  async type(text: string, delay = 48): Promise<void> {
    await this.page.keyboard.type(text, { delay });
  }

  async wheel(dy: number, notches = 6): Promise<void> {
    for (let i = 0; i < notches; i++) {
      await this.page.mouse.wheel(0, dy / notches);
      await sleep(0.05);
    }
  }
}

async function openContext(browser: Browser, signedIn: boolean): Promise<BrowserContext> {
  if (signedIn && !fs.existsSync(AUTH)) {
    throw new Error(`no saved session at ${AUTH}: run "npm run login" first`);
  }
  const context = await browser.newContext({
    baseURL: BASE,
    viewport: VIEWPORT,
    deviceScaleFactor: DPR,
    locale: 'en-US',
    timezoneId: 'Asia/Bangkok',
    colorScheme: 'light',
    serviceWorkers: 'block',
    ...(signedIn ? { storageState: AUTH } : {}),
  });
  // tsx (esbuild, keepNames) wraps named functions in a `__name(...)` helper that does not exist in the page: functions
  // handed to page.evaluate would throw without this no-op stand-in.
  await context.addInitScript({ content: 'globalThis.__name = (fn) => fn;' });
  // Start meeting opens the Meet link in a new tab; the recording stays on the app (the call is drawn by the composition).
  await context.addInitScript(() => {
    window.open = () => null;
  });
  // The whole page a little larger, as a person would zoom the browser for a recording. Chrome reports zoomed geometry, so
  // the pointer's targets and the screencast's pixels agree.
  await context.addInitScript((zoom: number) => {
    const apply = () => {
      document.documentElement.style.zoom = String(zoom);
    };
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', apply);
    } else {
      apply();
    }
  }, ZOOM);
  return context;
}

/** Paints one invisible change so the screencast sends a frame of a page that is otherwise still. */
async function kick(page: Page): Promise<void> {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        const dot = document.createElement('div');
        dot.style.cssText = 'position:fixed;left:0;top:0;width:2px;height:2px;background:#000;opacity:.02;z-index:2147483647;pointer-events:none';
        document.body.appendChild(dot);
        requestAnimationFrame(() => {
          requestAnimationFrame(() => {
            dot.remove();
            resolve();
          });
        });
      }),
  );
}

/** The interface in English, whatever the saved session last used (`kiwi.ui` in localStorage keeps the choice). */
async function ensureEnglish(page: Page): Promise<void> {
  const changed = await page.evaluate(() => {
    const raw = localStorage.getItem('kiwi.ui');
    if (raw === null) {
      return false;
    }
    const parsed = JSON.parse(raw) as { state?: { lang?: string } };
    if (parsed.state?.lang === undefined || parsed.state.lang === 'en') {
      return false;
    }
    parsed.state.lang = 'en';
    localStorage.setItem('kiwi.ui', JSON.stringify(parsed));
    return true;
  });
  if (changed) {
    await page.reload();
  }
}

/**
 * Simulates the Google side of the Calendar for the recording: ScheduleMeeting is answered with the event the form asked
 * for (with a made-up Meet link) instead of reaching the server, and ListUpcoming adds the simulated events to what the
 * server lists. The page cannot tell the difference; Google and the database are never touched.
 */
async function mockCalendar(context: BrowserContext): Promise<void> {
  // Start meeting: a Meet space is "created" without Google; the call itself is the composition's mocked call scene.
  await context.route('**/api/kiwi.meetings.v1.MeetingService/StartMeeting', async (route) => {
    const req = route.request().postDataJSON() as { channelId: string };
    await sleep(0.7);
    log('simulated StartMeeting');
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ meeting: { id: '00000000-0000-7000-8000-00000000a001', channelId: req.channelId, title: 'Q4 launch sync', meetUri: 'https://meet.google.com/kiw-demo-xyz' }, transcriptionAutomatic: true }),
    });
  });
  // Meetings history: only the finished meetings with their minutes. The workspace also holds test meetings that failed or
  // never got a transcript; they are real states of the product, but not what this demo is about.
  await context.route('**/api/kiwi.meetings.v1.MeetingService/ListMeetings', async (route) => {
    const response = await route.fetch();
    const body = (await response.json().catch(() => ({}))) as { meetings?: { status?: string }[] };
    const ready = (body.meetings ?? []).filter((meeting) => meeting.status === 'MEETING_STATUS_READY');
    // A team that uses K.I.W.I. has a history: three earlier finished meetings, shaped like the real one (only listed;
    // the take opens the real Q4 launch sync).
    const real = ready[0] as Record<string, unknown> | undefined;
    const earlier = real === undefined ? [] : [
      { title: 'Cross-dock partner review', ref: 'MTG-0998', daysAgo: 3 },
      { title: 'Launch marketing plan', ref: 'MTG-0997', daysAgo: 6 },
      { title: 'Weekly ops sync', ref: 'MTG-0996', daysAgo: 10 },
    ].map((m, i) => {
      const at = new Date(Date.now() - m.daysAgo * 86_400_000);
      at.setHours(10, 0, 0, 0);
      return { ...real, id: `00000000-0000-7000-8000-0000000b000${i}`, title: m.title, ref: m.ref, startedAt: at.toISOString(), endedAt: new Date(at.getTime() + 45 * 60_000).toISOString() };
    });
    const meetings = [...ready, ...earlier];
    await route.fulfill({ response, contentType: 'application/json', body: JSON.stringify({ ...body, meetings }) });
  });
  const simulated: Record<string, unknown>[] = [];
  await context.route('**/api/kiwi.calendar.v1.CalendarService/ScheduleMeeting', async (route) => {
    const req = route.request().postDataJSON() as { channelId: string; title: string; startAt: string; durationMinutes: number; orgWide?: boolean };
    const start = new Date(req.startAt);
    const end = new Date(start.getTime() + req.durationMinutes * 60_000);
    const event = {
      id: `00000000-0000-7000-8000-${String(simulated.length + 1).padStart(12, '0')}`,
      channelId: req.channelId,
      ref: `EVT-${1001 + simulated.length}`,
      title: req.title,
      startAt: start.toISOString(),
      endAt: end.toISOString(),
      meetUri: 'https://meet.google.com/kiw-demo-xyz',
      htmlLink: 'https://calendar.google.com/',
      status: 'EVENT_STATUS_SCHEDULED',
      orgWide: req.orgWide === true,
    };
    simulated.push(event);
    await sleep(0.8); // as long as Google takes, roughly
    log(`simulated ScheduleMeeting: ${req.title}`);
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ event }) });
  });
  await context.route('**/api/kiwi.calendar.v1.CalendarService/ListUpcoming', async (route) => {
    const response = await route.fetch();
    const body = (await response.json().catch(() => ({}))) as { events?: Record<string, unknown>[] };
    const events = [...(body.events ?? []), ...simulated];
    await route.fulfill({ response, contentType: 'application/json', body: JSON.stringify({ events }) });
  });
}

/**
 * Keeps the channel's meeting-summary slip (the minutes of Q4 launch sync) out of sight until `revealMinutes`, so the
 * recording can show it arriving when the mocked call ends. A watcher re-hides it whenever the stream re-renders.
 */
async function holdMinutes(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __minutesShown?: boolean; __minutesWatch?: MutationObserver };
    w.__minutesShown = false;
    const apply = () => {
      for (const article of document.querySelectorAll<HTMLElement>('[role="log"] article')) {
        const text = article.textContent ?? '';
        if (text.includes('meeting summary') && /REF MTG-\d+/.test(text)) {
          article.style.display = w.__minutesShown === true ? '' : 'none';
          article.dataset['minutes'] = '';
        }
      }
    };
    apply();
    w.__minutesWatch?.disconnect();
    w.__minutesWatch = new MutationObserver(apply);
    w.__minutesWatch.observe(document.body, { childList: true, subtree: true });
  });
}

/** Shows the minutes slip, scrolled into view, with a short highlight as it lands. */
async function revealMinutes(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __minutesShown?: boolean };
    w.__minutesShown = true;
    const slip = document.querySelector<HTMLElement>('article[data-minutes]');
    if (slip === null) {
      return;
    }
    slip.style.display = '';
    slip.scrollIntoView({ block: 'center', behavior: 'smooth' });
    slip.animate(
      [
        { opacity: 0, transform: 'translateY(18px)', boxShadow: '0 0 0 0 rgba(95,211,160,0)' },
        { opacity: 1, transform: 'translateY(0)', boxShadow: '0 0 0 6px rgba(95,211,160,.45)', offset: 0.35 },
        { opacity: 1, transform: 'translateY(0)', boxShadow: '0 0 0 0 rgba(95,211,160,0)' },
      ],
      { duration: 2200, easing: 'ease-out' },
    );
  });
}

/** The signed-in workspace page: a session that has expired lands on /signin instead, which is reported, not recorded. */
async function openWorkspace(context: BrowserContext, to = '/'): Promise<Page> {
  const page = await context.newPage();
  await page.goto(to);
  const nav = page.getByRole('navigation', { name: 'Views, channels and agents' });
  const signin = page.getByRole('button', { name: 'Continue with Google' });
  await nav.or(signin).first().waitFor({ timeout: 30_000 });
  if (await signin.isVisible()) {
    throw new Error('the saved session is not signed in any more: run "npm run login" again');
  }
  await ensureEnglish(page);
  await nav.waitFor();
  return page;
}

/** One Ask question off camera, so the first recorded one does not pay for a cold model. */
async function warmUp(browser: Browser): Promise<void> {
  log('warming the model up with one Ask question (not recorded)…');
  const context = await openContext(browser, true);
  try {
    const page = await openWorkspace(context, '/ask');
    await page.getByRole('textbox', { name: 'Ask a question, in English or Thai' }).fill(QUESTION_EN);
    await page.getByRole('button', { name: 'Ask', exact: true }).click();
    const thread = page.getByRole('log', { name: 'Questions and answers' });
    const answered = thread.locator('.kiwi-slip').filter({ hasText: /REF ANS-\d{4}/ }).first();
    const failed = thread.locator('[data-tone="failed"]').first();
    await answered
      .or(failed)
      .waitFor({ timeout: 240_000 })
      .catch(() => log('warm-up did not finish in 4 minutes; recording anyway'));
    if ((await failed.count()) > 0) {
      const text = (await failed.innerText()).replace(/\s+/g, ' ').trim().slice(0, 200);
      throw new Error(`the Ask warm-up failed, so the model is not answering; fix the stack before recording: ${text}`);
    }
    if ((await answered.count()) > 0) {
      log('warm-up answered');
    }
  } finally {
    await context.close();
  }
}

/** The weekday a week or so from today, as the calendar form wants it (local date parts). */
function rehearsalDate(): Date {
  const date = new Date();
  date.setDate(date.getDate() + 7);
  while (date.getDay() === 0 || date.getDay() === 6) {
    date.setDate(date.getDate() + 1);
  }
  date.setHours(14, 0, 0, 0);
  return date;
}

async function main(): Promise<void> {
  await fsp.rm(FRAMES, { recursive: true, force: true });
  await fsp.mkdir(FRAMES, { recursive: true });
  const browser = await chromium.launch({ headless: !HEADED });
  const rec = new Recorder();
  try {
    if (CHECK) {
      await warmUp(browser);
      return;
    }
    if (!PROBE && WARMUP) {
      await warmUp(browser);
    }

    // ---- Opening: the sign-in page, then the pointer goes to Continue with Google. ----
    const anon = await openContext(browser, false);
    let page = await anon.newPage();
    await page.goto('/signin');
    const signInButton = page.getByRole('button', { name: 'Continue with Google' });
    await signInButton.waitFor();
    await sleep(0.5);
    await rec.attach(page);
    await kick(page);
    rec.scene('signin');
    let hand = new Hand(page, rec, { x: VIEWPORT.width * 0.64, y: VIEWPORT.height * 0.62 });
    await sleep(PROBE ? 2 : 3.5);
    await hand.to(signInButton, { ax: 0.55 });
    await sleep(1.1);
    // The real click would hand the page to Google; the recording cuts to the workspace instead, so only the overlay shows the press.
    await hand.press(false);
    await sleep(0.7);
    await anon.close();
    if (PROBE) {
      await rec.finish();
      return;
    }

    // ---- Sign in: the workspace, and the rail's areas with their inner sidebars. ----
    const context = await openContext(browser, true);
    if (MOCK_GOOGLE) {
      await mockCalendar(context);
    }
    page = await openWorkspace(context);
    await holdMinutes(page);
    await rec.attach(page);
    await kick(page);
    rec.scene('workspace');
    hand = new Hand(page, rec, { x: hand.x, y: hand.y });
    const rail = page.getByRole('navigation', { name: 'Areas' });
    const inner = page.getByRole('navigation', { name: 'Views, channels and agents' });
    await sleep(2.5);
    for (const area of ['Workspace', 'Calendar', 'Agents', 'Chat']) {
      await hand.click(rail.getByRole('link', { name: area, exact: true }));
      await sleep(area === 'Chat' ? 1.2 : 2.2);
    }

    // ---- Chat: #q4-launch, a message in two languages, one sent while offline. ----
    rec.scene('chat');
    await hand.click(inner.getByRole('link', { name: 'q4-launch' }));
    const stream = page.getByRole('log', { name: 'Messages in #q4-launch' });
    await stream.waitFor();
    await holdMinutes(page);
    await sleep(2);
    const composer = page.getByRole('combobox', { name: 'Message #q4-launch' });
    await hand.click(composer);
    await sleep(0.4);
    await hand.type(CHAT_MESSAGE, 42);
    await sleep(0.5);
    await hand.click(page.getByRole('button', { name: 'Send' }));
    await stream.locator('article').filter({ hasText: 'Capacity note is updated' }).last().waitFor();
    await sleep(2.2);
    // Offline: the message waits and says so; back online it goes.
    await context.setOffline(true);
    await hand.click(composer);
    await hand.type(OFFLINE_MESSAGE, 42);
    await hand.click(page.getByRole('button', { name: 'Send' }));
    const waiting = stream.locator('article').filter({ hasText: OFFLINE_MESSAGE }).getByRole('status');
    await waiting.waitFor({ timeout: 10_000 }).catch(() => log('no waiting status was shown for the offline message'));
    await sleep(2.6);
    await context.setOffline(false);
    await waiting.waitFor({ state: 'hidden', timeout: 30_000 }).catch(() => log('the offline message did not settle within 30s'));
    await sleep(2.5);

    // ---- Meeting: Start meeting in the channel (simulated; the call is the composition's mocked call scene). ----
    rec.scene('meet');
    await hand.click(page.getByRole('button', { name: 'Start meeting' }));
    await sleep(2.6);

    // ---- Minutes: when the call ends, the meeting summary arrives in the channel. ----
    rec.scene('mom');
    await sleep(0.6);
    await revealMinutes(page);
    rec.event('chime');
    await sleep(2.6);
    const minutes = stream.locator('article[data-minutes]');
    for (const label of ['Decision', 'Open question']) {
      const row = minutes.getByText(label, { exact: true }).first();
      if ((await row.count()) > 0) {
        await hand.to(row, { ax: 0.5 });
        await sleep(2.2);
      }
    }
    const stamp = minutes.getByRole('button', { name: /^Transcript at / }).first();
    if ((await stamp.count()) > 0) {
      await hand.to(stamp);
      await sleep(1.8);
    }
    await sleep(1.2);

    // ---- Meetings: the list, Q4 launch sync, its summary, and a draft task accepted. ----
    rec.scene('meetings');
    await hand.click(rail.getByRole('link', { name: 'Workspace', exact: true }));
    await sleep(1.2);
    await hand.click(inner.getByRole('link', { name: 'Meetings', exact: true }));
    await page.getByRole('heading', { level: 1, name: 'Meetings' }).waitFor();
    await sleep(2.4);
    const rows = page.locator('.mtg-row');
    const wanted = rows.filter({ hasText: MEETING_TITLE });
    const row = (await wanted.count()) > 0 ? wanted.first() : rows.first();
    if ((await rows.count()) === 0) {
      log('WARNING: the Meetings list is empty');
    } else {
      await hand.click(row, { ax: 0.3 });
      await page.locator('.mtg-detail').waitFor({ timeout: 20_000 });
      await sleep(2.6);
      for (const selector of ['.mtg-overview', '.mtg-sec >> nth=0', '.mtg-sec >> nth=1']) {
        const section = page.locator(selector).first();
        if ((await section.count()) > 0) {
          await hand.to(section, { ax: 0.06, ay: 0.35 });
          await sleep(3);
        }
      }
      const drafts = page.getByRole('complementary', { name: 'Draft tasks' });
      await hand.to(drafts, { ax: 0.5, ay: 0.12 });
      await sleep(1.8);
      const acceptable = drafts.locator('li.mtg-draft').filter({ has: page.locator('button.mtg-draft-yes:enabled') });
      if ((await acceptable.count()) > 0) {
        const item = acceptable.first();
        const title = (await item.locator('.mtg-draft-ttl').innerText()).trim();
        await hand.to(item, { ax: 0.3 });
        await sleep(1.8);
        await hand.click(item.locator('button.mtg-draft-yes'));
        log(`accepted the draft: ${title}`);
        rec.event('chime');
        await sleep(3.2);
      } else {
        log('WARNING: no draft task with an owner to accept; the panel is only shown');
        await sleep(3);
      }
    }

    // ---- Board: the confirmed task in its lane. ----
    rec.scene('board');
    await hand.click(inner.getByRole('link', { name: 'Board', exact: true }));
    await page.getByRole('heading', { level: 1, name: 'Board' }).waitFor();
    await sleep(2.4);
    const lane = page.getByRole('group', { name: `To do in ${MEETING_TITLE}` });
    if ((await lane.count()) > 0) {
      await hand.to(lane.getByRole('listitem').first(), { ax: 0.4, ay: 0.4 });
    }
    await sleep(4);

    // ---- Ask: a cited answer, a citation pressed. ----
    rec.scene('ask');
    await hand.click(rail.getByRole('link', { name: 'Agents', exact: true }));
    await page.getByRole('heading', { level: 1, name: 'Ask' }).waitFor();
    await sleep(1.8);
    const askField = page.getByRole('textbox', { name: 'Ask a question, in English or Thai' });
    const askButton = page.getByRole('button', { name: 'Ask', exact: true });
    const thread = page.getByRole('log', { name: 'Questions and answers' });
    const slips = thread.locator('.kiwi-slip').filter({ hasText: /REF ANS-\d{4}/ });
    const failed = thread.locator('[data-tone="failed"]');
    await hand.click(askField);
    await sleep(0.3);
    await hand.type(QUESTION_EN, 50);
    await sleep(0.6);
    await hand.click(askButton);
    await slips
      .first()
      .or(failed.first())
      .waitFor({ timeout: 240_000 })
      .catch(() => log('WARNING: no answer arrived within 4 minutes'));
    if ((await slips.count()) > 0) {
      rec.event('chime');
    }
    if ((await failed.count()) > 0 || (await slips.count()) === 0) {
      // A failed or missing answer would be the centre of the scene: the take is not worth keeping.
      throw new Error('the recorded Ask question got no answer; take abandoned (run it again)');
    }
    await sleep(3);
    const marker = slips.first().getByRole('button', { name: /^Source 1: / }).first();
    if ((await marker.count()) > 0) {
      await hand.click(marker);
      await sleep(4.5);
    }
    if (THAI) {
      const before = await slips.count();
      await hand.click(askField);
      await hand.type(QUESTION_TH, 60);
      await hand.click(askButton);
      await slips
        .nth(before)
        .waitFor({ timeout: 240_000 })
        .catch(() => log('WARNING: no Thai answer arrived within 4 minutes'));
      await sleep(4);
    }

    // ---- Calendar: Month, Week and Day, then a meeting scheduled for the whole workspace. ----
    rec.scene('calendar');
    await hand.click(rail.getByRole('link', { name: 'Calendar', exact: true }));
    await page.getByRole('heading', { level: 1, name: 'Calendar' }).waitFor();
    await sleep(2.2);
    const views = page.getByRole('group', { name: 'Switch calendar view' });
    for (const view of ['Week', 'Day', 'Month']) {
      await hand.click(views.getByRole('button', { name: view, exact: true }));
      await sleep(view === 'Month' ? 1.2 : 1.8);
    }
    if (SCHEDULE) {
      await hand.click(page.getByRole('button', { name: 'Schedule meeting' }));
      const form = page.locator('#schedule-form');
      await form.waitFor();
      await sleep(0.8);
      await hand.click(form.getByLabel('Title'));
      await hand.type(EVENT_TITLE, 55);
      await sleep(0.4);
      const team = form.getByLabel('Team');
      await hand.to(team);
      const teams = await team.locator('option').allTextContents();
      await team.selectOption({ label: teams.includes('q4-launch') ? 'q4-launch' : (teams[0] ?? '') });
      await sleep(0.6);
      // The date field is made of segments (day, month, year in the interface's order): each is clicked and typed.
      const when = rehearsalDate();
      const segments = form.locator('.kiwi-date-seg');
      const kinds = await segments.evaluateAll((nodes) => nodes.map((node) => node.getAttribute('data-type') ?? ''));
      const value: Record<string, string> = { day: String(when.getDate()).padStart(2, '0'), month: String(when.getMonth() + 1).padStart(2, '0'), year: String(when.getFullYear()) };
      for (let i = 0; i < kinds.length; i++) {
        const text = value[kinds[i] ?? ''];
        if (text === undefined) {
          continue;
        }
        await hand.click(segments.nth(i));
        await page.keyboard.type(text, { delay: 70 });
        await sleep(0.2);
      }
      const time = form.getByLabel('Time');
      await hand.click(time);
      await time.fill('14:00');
      await sleep(0.4);
      const length = form.getByLabel('Length');
      await hand.to(length);
      await length.selectOption('45');
      await sleep(0.5);
      await hand.click(form.getByRole('checkbox'));
      await sleep(0.8);
      await hand.click(form.getByRole('button', { name: 'Schedule', exact: true }));
      const closed = form.waitFor({ state: 'hidden', timeout: 90_000 }).then(() => 'closed' as const);
      const error = form.locator('.cal-err').waitFor({ timeout: 90_000 }).then(() => 'error' as const);
      const outcome = await Promise.race([closed, error]).catch(() => 'timeout' as const);
      if (outcome === 'closed') {
        rec.event('chime');
        await sleep(1.2);
        const chip = page.locator('.mv-chip').filter({ hasText: EVENT_TITLE }).first();
        if ((await chip.count()) > 0) {
          await hand.to(chip, { ax: 0.3 });
        }
        await sleep(4);
      } else {
        log(`WARNING: scheduling did not succeed (${outcome}); the form is closed so the scene ends on the calendar`);
        await sleep(2.5);
        await hand.click(form.getByRole('button', { name: 'Close', exact: true }));
        await sleep(2.5);
      }
    } else {
      await sleep(4);
    }

    await rec.finish();
    await context.close();
  } finally {
    await browser.close();
  }
}

main().catch((error: unknown) => {
  console.error(error);
  process.exit(1);
});
