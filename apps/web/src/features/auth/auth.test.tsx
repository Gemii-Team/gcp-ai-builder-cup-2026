import { Code, ConnectError } from '@connectrpc/connect';
import { QueryClient } from '@tanstack/react-query';
import { createI18n } from '@kiwi/core';
import { act, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { hardNavigate } from '../../navigation';
import { UI_STORAGE_KEY, useUiStore } from '../../state/ui';
import { channel } from '../../testing/fixtures';
import { FakeRealtimeClient } from '../../testing/fakeRealtime';
import { renderRoutes } from '../../testing/renderRoutes';
import { createStubApi, seedApi } from '../../testing/stubApi';
import { fetchAuthConfig } from './authConfig';
import { ERROR_CODES, signInErrorNotice } from './errors';
import { safeReturnTo } from './returnTo';

// The signed-in shell opens its live connection through this factory: a test never opens a socket.
vi.mock('../../realtime/client', () => ({ createAppRealtimeClient: () => new FakeRealtimeClient() }));

const CONFIG = { workspaceName: 'Siam Riverside Trading', domain: 'siamriverside.co.th' };
const HEADING = { level: 1, name: 'Sign in to Siam Riverside Trading' };
const NO_CHANNELS = "You aren't in any channel yet.";
const RAIL = { name: 'Views, channels and agents' };

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });

/** What `GET /api/auth/config` answers; a plain `fetch`, since the endpoint is not in the generated client. */
function stubConfig(answer: () => Promise<Response> = () => Promise.resolve(json(CONFIG))) {
  const stub = vi.fn<typeof fetch>(() => answer());
  vi.stubGlobal('fetch', stub);
  return stub;
}

/** The session the server no longer knows: what GetMe answers a person who is not signed in. */
function signedOutApi() {
  const api = createStubApi();
  api.identity.getMe.mockRejectedValue(new ConnectError('no session', Code.Unauthenticated));
  return api;
}

function openSignIn(search = '') {
  return renderRoutes(`/signin${search}`, signedOutApi());
}

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
  stubConfig();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('Guard', () => {
  it('Guard_UnauthenticatedRedirectsWithReturnTo', async () => {
    const { router } = renderRoutes('/c/ch-q4?msg=12', signedOutApi());

    expect(await screen.findByRole('heading', HEADING)).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/signin');
    expect(router.state.location.search).toEqual({ return_to: '/c/ch-q4?msg=12' });
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument();
  });

  it('Guard_SignedInLeavesSignin', async () => {
    const { router } = renderRoutes('/signin', seedApi(createStubApi(), { channels: [] }));

    expect(await screen.findByText(NO_CHANNELS)).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
    expect(screen.queryByRole('heading', { name: /^Sign in/ })).not.toBeInTheDocument();
  });

  // The guards and the screens (rail, tab bar, account) ask through `meQuery`: a fresh session is one GetMe, not one per reader.
  it('asks GetMe once for the guard and every screen that shows the signed-in person', async () => {
    const api = seedApi(createStubApi(), { channels: [channel()] });
    const { router } = renderRoutes('/c/ch-q4', api, new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 30_000 } } }));

    expect(await screen.findByRole('navigation', RAIL)).toBeInTheDocument();
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/c/ch-q4');
    });
    expect(await screen.findByRole('button', { name: /Pimchanok S\./ })).toBeInTheDocument();
    expect(api.identity.getMe).toHaveBeenCalledTimes(1);
  });

  it('sends a signed-in person on to the page return_to names', async () => {
    const { router } = renderRoutes('/signin?return_to=%2Fc%2Fch-q4%3Fmsg%3D12', seedApi(createStubApi(), { channels: [channel()] }));

    await waitFor(() => {
      expect(router.state.location.href).toBe('/c/ch-q4?msg=12');
    });
    expect(await screen.findByRole('navigation', RAIL)).toBeInTheDocument();
  });

  // Prove-the-guard: return_to is a link anybody can craft, so a signed-in person must never be sent off the origin by it.
  it.each([
    '//evil.example/x',
    '///evil.example',
    'http://evil.example/x',
    'https://evil.example',
    'javascript:alert(1)',
    '/\\evil.example',
    '\\\\evil.example',
    '\\/evil.example',
    '/\t/evil.example',
    'c/ch-q4',
    '',
  ])('does not follow return_to=%j out of the app', async (target) => {
    const { router } = renderRoutes(`/signin?return_to=${encodeURIComponent(target)}`, seedApi(createStubApi(), { channels: [] }));

    expect(await screen.findByText(NO_CHANNELS)).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
  });

  it('guards every route of the signed-in layout, the start page included: a signed-out visit to / lands on the landing page', async () => {
    const { router } = renderRoutes('/', signedOutApi());

    expect(await screen.findByRole('heading', { level: 1, name: 'The AI workspace where conversations become decisions.' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/welcome');
    expect(screen.queryByRole('navigation', RAIL)).not.toBeInTheDocument();
  });

  it('lets a person sign in when the server cannot answer GetMe: the page shows, the button works', async () => {
    const api = createStubApi();
    api.identity.getMe.mockRejectedValue(new ConnectError('down', Code.Unavailable));
    renderRoutes('/signin', api);

    expect(await screen.findByRole('button', { name: 'Continue with Google' })).toBeEnabled();
  });
});

describe('safeReturnTo', () => {
  it.each(['/', '/c/ch-q4', '/c/ch-q4?msg=12', '/board?channel=0194f3a2#top', '/ask?q=%E0%B8%AA'])('keeps the same-origin path %s', (path) => {
    expect(safeReturnTo(path)).toBe(path);
  });

  it.each([
    undefined,
    '',
    'c/ch-q4',
    ' //evil.example',
    '//evil.example',
    '/\\evil.example',
    '\\\\evil.example',
    '\\/evil.example',
    '/\t/evil.example',
    '/\n/evil.example',
    'http://evil.example',
    'https://evil.example/c',
    'javascript:alert(1)',
    'data:text/html,x',
  ])('rejects %j', (path) => {
    expect(safeReturnTo(path)).toBeUndefined();
  });
});

describe('error codes', () => {
  it('ErrorCodes_MapToMessagesInEnglishAndThai', () => {
    const keys = new Set<string>();
    for (const code of ERROR_CODES) {
      const { key } = signInErrorNotice(code, {});
      keys.add(key);
      const english = createI18n('en').plain(key, { workspace: 'Siam Riverside Trading' });
      const thai = createI18n('th').plain(key, { workspace: 'Siam Riverside Trading' });
      expect(english, `${code} in English`).toMatch(/^[\x20-\x7E’]+$/);
      expect(thai, `${code} in Thai`).toMatch(/[\u0E00-\u0E7F]/);
    }
    expect([...keys]).toHaveLength(ERROR_CODES.length);
  });

  it('names the rejected address only for domain_not_allowed and not_invited, and the workspace only when there is one', () => {
    expect(signInErrorNotice('domain_not_allowed', { email: 'pim@gmail.com', workspaceName: 'Siam Riverside Trading' })).toEqual({
      key: 'signInNotInWorkspace',
      email: 'pim@gmail.com',
    });
    expect(signInErrorNotice('domain_not_allowed', { email: 'pim@gmail.com', workspaceName: '' })).toEqual({
      key: 'signInNotInThisWorkspace',
      email: 'pim@gmail.com',
    });
    expect(signInErrorNotice('domain_not_allowed', { workspaceName: 'Siam Riverside Trading' })).toEqual({ key: 'signInErrorDomainNotAllowed' });
    expect(signInErrorNotice('not_invited', { email: 'pim@gmail.com', workspaceName: 'Siam Riverside Trading' })).toEqual({
      key: 'signInNotInvited',
      email: 'pim@gmail.com',
    });
    expect(signInErrorNotice('not_invited', { workspaceName: 'Siam Riverside Trading' })).toEqual({ key: 'signInErrorNotInvited' });
    expect(signInErrorNotice('access_denied', { email: 'pim@gmail.com', workspaceName: 'Siam Riverside Trading' })).toEqual({ key: 'signInErrorAccessDenied' });
  });

  it('answers a code it does not know like a failed exchange, never with silence', () => {
    expect(signInErrorNotice('server_error', {})).toEqual(signInErrorNotice('exchange_failed', {}));
  });

  it.each([
    ['state_mismatch', 'That sign-in expired. Try again.'],
    ['domain_not_allowed', 'Use your company Google Workspace account.'],
    ['access_denied', 'Google sign-in was cancelled. Try again.'],
    ['exchange_failed', "Couldn't finish signing in with Google. Try again."],
    ['not_invited', 'This account has not been invited to this workspace. Ask someone who is in it to add you.'],
    ['server_error', "Couldn't finish signing in with Google. Try again."],
  ])('shows ?error=%s above the button, as an alert', async (code, message) => {
    openSignIn(`?error=${code}`);

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(message);
    const button = screen.getByRole('button', { name: 'Continue with Google' });
    expect(alert.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('SignIn_DomainNotAllowedNamesTheRejectedAddressFromTheEmailParam', async () => {
    openSignIn('?error=domain_not_allowed&email=pim%40gmail.com');

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("pim@gmail.com isn't part of Siam Riverside Trading's Google Workspace. Sign in with your work account.");
    expect(within(alert).getByText('pim@gmail.com')).toHaveClass('font-mono');
    expect(within(alert).getByText('pim@gmail.com')).toHaveAttribute('lang', 'en');
  });

  it('names the address in Thai too, with the Latin words tagged', async () => {
    useUiStore.getState().setLang('th');
    openSignIn('?error=domain_not_allowed&email=pim%40gmail.com');

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('pim@gmail.com ไม่ได้อยู่ใน Google Workspace ของ Siam Riverside Trading ลงชื่อเข้าใช้ด้วยบัญชีที่ทำงานของคุณ');
    expect(within(alert).getByText('Google Workspace')).toHaveAttribute('lang', 'en');
    expect(within(alert).getByText('Siam Riverside Trading')).toHaveAttribute('lang', 'en');
  });

  it('does not say the address when the server named none, and ignores one that came with another code', async () => {
    openSignIn('?error=access_denied&email=pim%40gmail.com');

    expect(await screen.findByRole('alert')).not.toHaveTextContent('pim@gmail.com');
  });

  it('names no workspace when the server has none to name', async () => {
    stubConfig(() => Promise.resolve(json({ workspaceName: '', domain: '' })));
    openSignIn('?error=domain_not_allowed&email=pim%40gmail.com');

    expect(await screen.findByRole('alert')).toHaveTextContent("pim@gmail.com isn't part of this Google Workspace. Sign in with your work account.");
  });

  it('SignIn_NotInvitedNamesTheAddressInMonoAndSaysWhoCanAddThem', async () => {
    openSignIn('?error=not_invited&email=pim%40gmail.com');

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('pim@gmail.com has not been invited to this workspace. Ask someone who is in it to add you.');
    expect(within(alert).getByText('pim@gmail.com')).toHaveClass('font-mono');
    expect(within(alert).getByText('pim@gmail.com')).toHaveAttribute('lang', 'en');
  });

  it('says not_invited in Thai, whole, with the address as the only Latin run', async () => {
    useUiStore.getState().setLang('th');
    openSignIn('?error=not_invited&email=pim%40gmail.com');

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('pim@gmail.com ยังไม่ได้รับเชิญเข้าร่วมพื้นที่ทำงานนี้ ขอให้สมาชิกในพื้นที่ทำงานเพิ่มคุณ');
    expect(within(alert).getByText('pim@gmail.com')).toHaveAttribute('lang', 'en');
    expect(within(alert).getByText('pim@gmail.com')).toHaveClass('font-mono');
  });

  it('does not leave not_invited without a sentence when the server named no address', async () => {
    openSignIn('?error=not_invited');

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('This account has not been invited to this workspace. Ask someone who is in it to add you.');
    expect(within(alert).queryByText(/@/)).toBeNull();
  });

  describe('the address the query names', () => {
    const LONGEST = `${'a'.repeat(242)}@example.com`;

    it.each([
      'pim@gmail.com',
      'pim+launch@siamriverside.co.th',
      'pim.k@siamriverside.co.th',
      'ปิม@siamriverside.co.th',
      LONGEST,
    ])('shows %s, an address of at most 254 characters', (email) => {
      expect(LONGEST).toHaveLength(254);
      expect(signInErrorNotice('not_invited', { email })).toEqual({ key: 'signInNotInvited', email });
      expect(signInErrorNotice('domain_not_allowed', { email, workspaceName: '' })).toMatchObject({ email });
    });

    it.each([
      ['an address of 255 characters', `a${LONGEST}`],
      ['a long crafted value', 'x'.repeat(5000)],
      ['text that is not an address', 'Call 0800-000-000 to claim your prize'],
      ['an empty local part', '@gmail.com'],
      ['an empty domain', 'pim@'],
      ['a domain without a dot', 'pim@gmail'],
      ['two @', 'pim@evil@gmail.com'],
      ['a space', 'pim k@gmail.com'],
      ['a line break', 'pim@gmail.com\nVisit evil.example'],
      ['a right-to-left override', 'pim@\u202Emoc.liamg'],
      ['a zero-width space', 'pim@gm\u200Bail.com'],
      ['a NUL', 'pim@gmail.com\u0000'],
    ])('says no address for %s', (_name, email) => {
      expect(signInErrorNotice('not_invited', { email })).toEqual({ key: 'signInErrorNotInvited' });
      expect(signInErrorNotice('domain_not_allowed', { email, workspaceName: 'Siam Riverside Trading' })).toEqual({ key: 'signInErrorDomainNotAllowed' });
    });

    it.each([
      ['a crafted long value', 'x'.repeat(300), /xxx/],
      ['text that is not an address', 'Call%200800-000-000%20now%20for%20your%20prize', /Call 0800/],
      ['two @', 'a%40b%40c.com', /a@b@c/],
    ])('SignIn_NotInvitedWithACraftedAddress_ShowsTheNoticeWithoutAnyAddress (%s)', async (_name, email, crafted) => {
      openSignIn(`?error=not_invited&email=${email}`);

      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent('This account has not been invited to this workspace. Ask someone who is in it to add you.');
      expect(alert).not.toHaveTextContent(crafted);
      expect(alert.querySelector('.font-mono')).toBeNull();
    });
  });
});

describe('the sign-in screen', () => {
  it.each(['en', 'th'] as const)('tags the English words of the Demo data label lang="en" and the Thai ones lang="th", whatever the language of the page (%s)', async (lang) => {
    useUiStore.getState().setLang(lang);
    openSignIn();

    expect(await screen.findByText('Demo data')).toHaveAttribute('lang', 'en');
    expect(screen.getByText('ข้อมูลตัวอย่าง')).toHaveAttribute('lang', 'th');
  });

  it('LanguageSwitch_SetsHtmlLangAndPersists', async () => {
    const { user } = openSignIn();
    const group = await screen.findByRole('group', { name: 'Language' });
    expect(within(group).getByRole('button', { name: 'English' })).toHaveAttribute('aria-pressed', 'true');
    expect(document.title).toBe('Sign in · K.I.W.I.');

    await user.click(within(group).getByRole('button', { name: 'ไทย' }));

    expect(document.documentElement.lang).toBe('th');
    expect(document.title).toBe('ลงชื่อเข้าใช้ · K.I.W.I.');
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('ลงชื่อเข้าใช้ Siam Riverside Trading');
    expect(screen.getByRole('button', { name: 'ดำเนินการต่อด้วย Google' })).toBeInTheDocument();
    expect(screen.getByRole('group', { name: 'ภาษา' })).toBeInTheDocument();
    const stored = JSON.parse(localStorage.getItem(UI_STORAGE_KEY) ?? '{}') as { state?: { lang?: string } };
    expect(stored.state?.lang).toBe('th');

    await user.click(within(screen.getByRole('group', { name: 'ภาษา' })).getByRole('button', { name: 'English' }));

    expect(document.documentElement.lang).toBe('en');
    expect(document.title).toBe('Sign in · K.I.W.I.');
    expect(screen.getByRole('heading', HEADING)).toBeInTheDocument();
  });

  it('SignIn_ButtonTargetsLoginWithReturnTo', async () => {
    const go = vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const first = openSignIn('?return_to=%2Fc%2Fch-q4%3Fmsg%3D12');

    await first.user.click(await screen.findByRole('button', { name: 'Continue with Google' }));

    expect(go).toHaveBeenCalledWith('/api/auth/login?return_to=%2Fc%2Fch-q4%3Fmsg%3D12');
  });

  it.each([
    ['', '%2F'],
    ['?return_to=%2F%2Fevil.example', '%2F'],
    ['?return_to=http%3A%2F%2Fevil.example', '%2F'],
  ])('sends %j to the server asking to come back to the start page: a return_to that leaves the app is dropped', async (search, expected) => {
    const go = vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const { user } = openSignIn(search);

    await user.click(await screen.findByRole('button', { name: 'Continue with Google' }));

    expect(go).toHaveBeenCalledWith(`/api/auth/login?return_to=${expected}`);
  });

  it('Loading_DisablesButtonSetsAriaBusyAndShowsPending', async () => {
    const go = vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const { user } = openSignIn();
    const button = await screen.findByRole('button', { name: 'Continue with Google' });
    expect(button).toBeEnabled();
    expect(button).toHaveAttribute('aria-busy', 'false');
    expect(screen.queryByRole('status')).not.toBeInTheDocument();

    await user.click(button);

    expect(button).toBeDisabled();
    expect(button).toHaveAttribute('aria-busy', 'true');
    expect(screen.getByRole('status')).toHaveTextContent('Redirecting to Google…');
    expect(go).toHaveBeenCalledTimes(1);
  });

  it('is ready to sign in again when the browser shows the page from its back-forward cache', async () => {
    vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const { user } = openSignIn();
    const button = await screen.findByRole('button', { name: 'Continue with Google' });
    await user.click(button);
    expect(button).toBeDisabled();

    act(() => {
      window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
    });

    expect(button).toBeEnabled();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('Thai_LatinWordsInsideThaiStringsCarryLangEn', async () => {
    useUiStore.getState().setLang('th');
    const { user } = openSignIn();
    vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    await screen.findByRole('heading', { level: 1, name: 'ลงชื่อเข้าใช้ Siam Riverside Trading' });
    await user.click(screen.getByRole('button', { name: 'ดำเนินการต่อด้วย Google' }));

    const latin = (text: string) => screen.getAllByText(text).filter((element) => element.getAttribute('lang') === 'en');
    for (const word of ['Siam Riverside Trading', 'Google', 'K.I.W.I.', 'AI', 'Meet', 'Drive', 'Gmail']) {
      expect(latin(word).length, word).toBeGreaterThan(0);
    }
    // Every Latin word of a Thai string is tagged; none is left loose beside Thai prose.
    expect(screen.getByRole('status')).toHaveTextContent('กำลังเปลี่ยนเส้นทางไปยัง Google');
    expect(within(screen.getByRole('status')).getByText('Google')).toHaveAttribute('lang', 'en');
  });

  it('SignIn_NoRailNoTabBar', async () => {
    openSignIn();

    await screen.findByRole('heading', HEADING);
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument();
    expect(document.querySelector('.shell, .rail, .tabs')).toBeNull();
    expect(screen.getByRole('banner')).toContainElement(screen.getByRole('group', { name: 'Language' }));
    expect(screen.getByRole('main')).toContainElement(screen.getByRole('heading', HEADING));
    expect(document.querySelector('meta[name="theme-color"]')).toHaveAttribute('content', '#E6F2EE');
  });

  it('SignIn_HeadingAndHintComeFromAuthConfigAndFallBackWhenEmpty', async () => {
    const config = stubConfig();
    openSignIn();

    expect(await screen.findByRole('heading', HEADING)).toBeInTheDocument();
    expect(screen.getByText(/^Use your/)).toHaveTextContent('Use your @siamriverside.co.th Google account.');
    expect(screen.getByText('@siamriverside.co.th')).toHaveClass('font-mono');
    expect(config).toHaveBeenCalledTimes(1);
    expect(config).toHaveBeenCalledWith('/api/auth/config', expect.objectContaining({ credentials: 'same-origin' }));
  });

  it.each([
    ['empty names', () => Promise.resolve(json({ workspaceName: '', domain: '' }))],
    ['no names at all', () => Promise.resolve(json({}))],
    ['a server error', () => Promise.resolve(new Response('bad gateway', { status: 502 }))],
    ['an answer that is not JSON', () => Promise.resolve(new Response('<html>', { status: 200 }))],
    ['an unreachable server', () => Promise.reject(new TypeError('Failed to fetch'))],
  ])('falls back to the default heading and shows no hint with %s, and still lets the person sign in', async (_name, answer) => {
    stubConfig(answer);
    openSignIn();

    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in to K.I.W.I.' })).toBeInTheDocument();
    expect(screen.queryByText(/Use your/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue with Google' })).toBeEnabled();
  });

  it('shows the signed-out notice above the heading after sign-out', async () => {
    openSignIn('?signedout=1');

    const notice = await screen.findByRole('status');
    expect(notice).toHaveTextContent("You've signed out.");
    expect(notice.compareDocumentPosition(screen.getByRole('heading', HEADING)) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('says what K.I.W.I. will see, the promise to ask first set apart', async () => {
    openSignIn();

    expect(await screen.findByRole('heading', { level: 2, name: 'What K.I.W.I. will see' })).toBeInTheDocument();
    expect(screen.getAllByRole('listitem').map((item) => item.textContent)).toEqual([
      'Channels you join and messages in them',
      'Files you drop into a channel',
      'Meet transcripts from meetings you start in K.I.W.I.',
      'K.I.W.I. asks before it reads Drive or drafts Gmail.',
    ]);
  });
});

describe('fetchAuthConfig', () => {
  it('reads the two names from the same-origin endpoint', async () => {
    const config = stubConfig();

    await expect(fetchAuthConfig()).resolves.toEqual(CONFIG);
    expect(config).toHaveBeenCalledWith('/api/auth/config', expect.objectContaining({ credentials: 'same-origin' }));
  });

  it('keeps only strings: anything else is an empty name', async () => {
    stubConfig(() => Promise.resolve(json({ workspaceName: 7, domain: null })));

    await expect(fetchAuthConfig()).resolves.toEqual({ workspaceName: '', domain: '' });
  });
});

describe('when the session or the channels cannot be loaded', () => {
  it('shows an error with Retry when GetMe fails for another reason than the session, and the shell returns on Retry', async () => {
    const api = seedApi(createStubApi(), { channels: [] });
    api.identity.getMe.mockRejectedValueOnce(new ConnectError('down', Code.Unavailable));
    const { user } = renderRoutes('/', api);

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't load K.I.W.I. Check your connection and try again.");
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument();

    await user.click(within(alert).getByRole('button', { name: 'Retry' }));

    expect(await screen.findByText(NO_CHANNELS)).toBeInTheDocument();
    expect(screen.getByRole('navigation', RAIL)).toBeInTheDocument();
  });

  it('shows the start page an error with Retry, and the rail one too, when ListChannels fails', async () => {
    const api = seedApi(createStubApi(), { channels: [channel()] });
    api.chat.listChannels.mockRejectedValueOnce(new ConnectError('down', Code.Unavailable));
    const { user, router } = renderRoutes('/', api);

    const failure = await within(await screen.findByRole('main')).findByRole('alert');
    expect(failure).toHaveTextContent("Couldn't load your channels.");
    expect(screen.queryByText('Loading…')).not.toBeInTheDocument();
    expect(within(screen.getByRole('navigation', RAIL)).getByRole('alert')).toHaveTextContent("Couldn't load your channels.");

    await user.click(within(failure).getByRole('button', { name: 'Retry' }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/c/ch-q4');
    });
    expect(within(screen.getByRole('navigation', RAIL)).queryByRole('alert')).not.toBeInTheDocument();
  });

  it('leaves for sign-in, with the page as return_to, when a call of the shell is answered Unauthenticated', async () => {
    const leave = vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const api = seedApi(createStubApi(), { channels: [channel()] });
    api.chat.listChannels.mockRejectedValue(new ConnectError('expired', Code.Unauthenticated));
    renderRoutes('/c/ch-q4?msg=12', api);

    await waitFor(() => {
      expect(leave).toHaveBeenCalledWith('/signin?return_to=%2Fc%2Fch-q4%3Fmsg%3D12');
    });
    expect(leave).toHaveBeenCalledTimes(1);
  });
});
