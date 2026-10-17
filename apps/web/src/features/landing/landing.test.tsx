import { Code, ConnectError } from '@connectrpc/connect';
import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { FakeRealtimeClient } from '../../testing/fakeRealtime';
import { renderRoutes } from '../../testing/renderRoutes';
import { createStubApi, seedApi } from '../../testing/stubApi';

// The signed-in shell opens its live connection through this factory: a test never opens a socket.
vi.mock('../../realtime/client', () => ({ createAppRealtimeClient: () => new FakeRealtimeClient() }));

const HEADLINE = { level: 1, name: 'The AI workspace where conversations become decisions.' };

function signedOutApi() {
  const api = createStubApi();
  api.identity.getMe.mockRejectedValue(new ConnectError('no session', Code.Unauthenticated));
  return api;
}

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
  // The sign-in page the links lead to reads its names from /api/auth/config.
  vi.stubGlobal('fetch', vi.fn<typeof fetch>(() => Promise.resolve(new Response(JSON.stringify({ workspaceName: '', domain: '' }), { status: 200 }))));
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('the landing page', () => {
  it('Landing_SignedOutRootShowsTheLandingPage', async () => {
    const { router } = renderRoutes('/', signedOutApi());

    expect(await screen.findByRole('heading', HEADLINE)).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/welcome');
    expect(document.title).toBe('K.I.W.I. · The AI workspace for your team');
  });

  it('keeps a deep link going straight to sign-in, with the page as return_to', async () => {
    const { router } = renderRoutes('/board', signedOutApi());

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/signin');
    });
    expect(router.state.location.search).toEqual({ return_to: '/board' });
  });

  it('leads every way in to sign-in: the top bar, the hero and the final call', async () => {
    renderRoutes('/welcome', signedOutApi());
    await screen.findByRole('heading', HEADLINE);

    const toSignIn = screen.getAllByRole('link').filter((link) => link.getAttribute('href') === '/signin');
    expect(toSignIn.map((link) => link.textContent)).toEqual(['Sign in', 'Get started with Google', 'Get started with Google']);
  });

  it('opens sign-in from Get started, inside the app', async () => {
    const { router, user } = renderRoutes('/welcome', signedOutApi());
    const hero = (await screen.findByRole('heading', HEADLINE)).parentElement!;

    await user.click(within(hero).getByRole('link', { name: 'Get started with Google' }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/signin');
    });
  });

  it('shows the six features, the three steps and the principle', async () => {
    renderRoutes('/welcome', signedOutApi());
    await screen.findByRole('heading', HEADLINE);

    expect(screen.getAllByRole('heading', { level: 3 }).map((heading) => heading.textContent)).toEqual([
      'Minutes of every meeting',
      'The AI proposes, you decide',
      'Answers with their sources',
      'Real-time chat that works offline',
      'A calendar with Meet built in',
      'Private by design',
      'Talk and meet',
      'K.I.W.I. drafts',
      'You confirm',
    ]);
    expect(screen.getByText('Nothing takes effect until a person confirms it.')).toBeInTheDocument();
    // The product preview is one labelled picture to assistive technology.
    expect(screen.getByRole('img', { name: /An example of K\.I\.W\.I\. at work/ })).toBeInTheDocument();
  });

  it('speaks Thai when the interface is in Thai', async () => {
    useUiStore.setState({ lang: 'th' });
    renderRoutes('/welcome', signedOutApi());

    expect(await screen.findByRole('heading', { level: 1, name: 'พื้นที่ทำงาน AI ที่เปลี่ยนบทสนทนาให้เป็นการตัดสินใจ' })).toBeInTheDocument();
  });

  it('sends a person who is signed in on to the app', async () => {
    const { router } = renderRoutes('/welcome', seedApi(createStubApi(), { channels: [] }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/');
    });
    expect(screen.queryByRole('heading', HEADLINE)).not.toBeInTheDocument();
  });
});
