import { Code, ConnectError } from '@connectrpc/connect';
import { screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { channel, task } from '../../testing/fixtures';
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
const TASKS = [task({ id: 't-q4', title: 'In the launch channel' }), task({ id: 't-ops', channelId: 'ch-ops', title: 'In the warehouse channel', ref: 'TSK-2036' })];

describe('the /board route', () => {
  it('shows the Board in the signed-in shell', async () => {
    const { router } = renderRoutes('/board', seedApi(createStubApi(), { channels: CHANNELS, tasks: TASKS }));

    expect(await screen.findByRole('heading', { level: 1, name: 'Board' })).toBeInTheDocument();
    expect(await screen.findByRole('heading', { level: 3, name: 'In the launch channel' })).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: 'Views, channels and agents' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/board');
  });

  it('starts the Channel filter on ?channel=<id>', async () => {
    renderRoutes('/board?channel=ch-ops', seedApi(createStubApi(), { channels: CHANNELS, tasks: TASKS }));

    expect(await screen.findByRole('heading', { level: 3, name: 'In the warehouse channel' })).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Channel' })).toHaveValue('ch-ops');
    expect(screen.queryByText('In the launch channel')).not.toBeInTheDocument();
  });

  it('sends nobody signed in to /signin, and back here once they are', async () => {
    const api = createStubApi();
    api.identity.getMe.mockRejectedValue(new ConnectError('no session', Code.Unauthenticated));

    const { router } = renderRoutes('/board?channel=ch-ops', api);

    expect(await screen.findByRole('heading', { name: /^Sign in/ })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/signin');
    expect(router.state.location.search).toEqual({ return_to: '/board?channel=ch-ops' });
  });
});
