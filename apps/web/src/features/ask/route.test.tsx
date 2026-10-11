import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { StartRunResponseSchema } from '@kiwi/api-client';
import { screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { channel, run } from '../../testing/fixtures';
import { FakeRealtimeClient } from '../../testing/fakeRealtime';
import { renderRoutes } from '../../testing/renderRoutes';
import { createStubApi, seedApi } from '../../testing/stubApi';

// The signed-in shell opens its live connection through this factory: a test never opens a socket.
vi.mock('../../realtime/client', () => ({ createAppRealtimeClient: () => new FakeRealtimeClient() }));

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
  vi.stubGlobal('fetch', vi.fn<typeof fetch>(() => Promise.resolve(new Response(JSON.stringify({ workspaceName: 'Siam Riverside Trading', domain: 'siamriverside.co.th' })))));
});

const CHANNELS = [channel({ id: 'ch-q4', name: 'q4-launch' }), channel({ id: 'ch-ops', name: 'ops-warehouse' })];

describe('the /ask route', () => {
  it('shows Ask in the signed-in shell, with its subtitle and Board as a secondary link', async () => {
    const { router } = renderRoutes('/ask', seedApi(createStubApi(), { channels: CHANNELS }));

    expect(await screen.findByRole('heading', { level: 1, name: 'Ask' })).toBeInTheDocument();
    expect(screen.getByText('Cited answers from your channels')).toBeInTheDocument();
    expect(await screen.findByText(/Searching/)).toHaveTextContent("Searching 2 channels you're in");
    expect(screen.getAllByRole('link', { name: 'Board' }).some((link) => link.getAttribute('href') === '/board')).toBe(true);
    expect(screen.getByRole('navigation', { name: 'Views, channels and agents' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/ask');
  });

  it('scopes the questions to ?channel=<id>: the line names the channel and StartRun carries it', async () => {
    const api = seedApi(createStubApi(), { channels: CHANNELS });
    api.agents.startRun.mockResolvedValue(create(StartRunResponseSchema, { run: run() }));
    const { user } = renderRoutes('/ask?channel=ch-ops', api);

    expect(await screen.findByText('Searching only #ops-warehouse')).toBeInTheDocument();
    expect(screen.getAllByRole('link', { name: 'Board' }).some((link) => link.getAttribute('href') === '/board?channel=ch-ops')).toBe(true);
    await user.type(screen.getByRole('textbox', { name: 'Ask a question, in English or Thai' }), 'Is the warehouse free?{Enter}');

    expect(api.agents.startRun.mock.calls[0]?.[0]).toEqual({ agent: 'assistant', question: 'Is the warehouse free?', channelId: 'ch-ops' });
    expect(await screen.findByRole('status', { name: 'Answer progress' })).toHaveTextContent('Retrieving from 1 channel…');
  });

  it('ignores a channel the person is not in, and one that is not text', async () => {
    renderRoutes('/ask?channel=ch-secret', seedApi(createStubApi(), { channels: CHANNELS }));

    expect(await screen.findByText(/Searching/)).toHaveTextContent("Searching 2 channels you're in");
  });

  it('sends nobody signed in to /signin, and back here once they are', async () => {
    const api = createStubApi();
    api.identity.getMe.mockRejectedValue(new ConnectError('no session', Code.Unauthenticated));

    const { router } = renderRoutes('/ask?channel=ch-ops', api);

    expect(await screen.findByRole('heading', { name: /^Sign in/ })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/signin');
    expect(router.state.location.search).toEqual({ return_to: '/ask?channel=ch-ops' });
  });
});
