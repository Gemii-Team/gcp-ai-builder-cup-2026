import { create } from '@bufbuild/protobuf';
import { ListChannelsResponseSchema, MembershipChange, MembershipChangedSchema } from '@kiwi/api-client';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { hardNavigate } from '../../navigation';
import { useUiStore } from '../../state/ui';
import { channel, frame, message, ME_ID } from '../../testing/fixtures';
import { createStubApi, seedApi } from '../../testing/stubApi';
import { renderApp } from '../../testing/render';

const RAIL = { name: 'Views, channels and agents' };

const CHANNELS = [channel({ id: 'ch-q4', name: 'q4-launch', lastSeq: 5n }), channel({ id: 'ch-sales', name: 'sales-th', lastSeq: 8n })];

/** The routes a channel link of the rail leads to. */
const CHANNEL_ROUTE = { path: '/c/$channelId', element: <p>Channel screen</p> };

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function shellWithChannels(path: string) {
  const api = seedApi(createStubApi(), { channels: CHANNELS });
  return renderApp({ path, api, routes: [CHANNEL_ROUTE] });
}

const AREAS = { name: 'Areas' };

describe('Rail', () => {
  it('lights the active area and marks the current route inside the inner sidebar', async () => {
    const { user } = shellWithChannels('/board');
    const areas = within(await screen.findByRole('navigation', AREAS));
    const inner = within(await screen.findByRole('navigation', RAIL));

    // On the Board, the Workspace area is lit and its Board link is the current route.
    expect(areas.getByRole('link', { name: 'Workspace' })).toHaveAttribute('data-active');
    expect(inner.getByRole('link', { name: 'Board' })).toHaveAttribute('aria-current', 'page');

    // Switching to the Agents area from the thin rail opens Ask and swaps the inner sidebar.
    await user.click(areas.getByRole('link', { name: 'Agents' }));
    await waitFor(() => {
      expect(inner.getByRole('link', { name: 'Ask' })).toHaveAttribute('aria-current', 'page');
    });
    expect(inner.queryByRole('link', { name: 'Board' })).not.toBeInTheDocument();

    // And the Chat area opens the channel last open.
    await user.click(areas.getByRole('link', { name: 'Chat' }));
    await waitFor(() => {
      expect(screen.getByText('Channel screen')).toBeInTheDocument();
    });
  });

  it('shows the channels in Chat and the agent in Agents, with the workspace on the account', async () => {
    const { user } = shellWithChannels('/c/ch-q4');
    const inner = within(await screen.findByRole('navigation', RAIL));

    // Chat area: the channels, and the workspace name on the account button at the foot.
    expect(await inner.findByRole('link', { name: 'q4-launch' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Pimchanok S\./ })).toHaveTextContent('Siam Riverside Trading');

    // Agents area: the assistant, shown online.
    await user.click(within(screen.getByRole('navigation', AREAS)).getByRole('link', { name: 'Agents' }));
    expect(await screen.findByText('@assistant')).toBeInTheDocument();
    expect(screen.getByText('online')).toBeInTheDocument();
  });

  it('counts only what arrives after the channel list was first seen as unread, and says so in the pill', async () => {
    const { client } = shellWithChannels('/c/ch-q4');
    const rail = within(await screen.findByRole('navigation', RAIL));
    await rail.findByRole('link', { name: 'sales-th' });
    expect(rail.queryByLabelText(/unread/)).not.toBeInTheDocument();

    await act(async () => {
      client.emit(frame({ case: 'messageCreated', value: message({ seq: 9, channelId: 'ch-sales', body: 'Hello' }) }));
      await Promise.resolve();
    });

    expect(await rail.findByLabelText('1 unread message')).toHaveTextContent('1');
    expect(rail.getByRole('link', { name: /sales-th/ })).toHaveAttribute('data-unread');
  });

  it('speaks Thai when the language is Thai, and tags the document', async () => {
    useUiStore.getState().setLang('th');
    shellWithChannels('/board');

    const inner = within(await screen.findByRole('navigation', { name: 'มุมมอง ช่องสนทนา และเอเจนต์' }));

    expect(inner.getByRole('link', { name: 'บอร์ด' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'พื้นที่งาน' })).toBeInTheDocument();
    expect(document.documentElement.lang).toBe('th');
  });

  it.each(['en', 'th'] as const)('tags the English words of the Demo data label lang="en" and the Thai ones lang="th", whatever the language of the page (%s)', async (lang) => {
    useUiStore.getState().setLang(lang);
    shellWithChannels('/c/ch-q4');

    expect(await screen.findByText('Demo data')).toHaveAttribute('lang', 'en');
    expect(screen.getByText('ข้อมูลตัวอย่าง')).toHaveAttribute('lang', 'th');
  });
});

describe('RealtimeProvider in the shell', () => {
  it('connects once and follows every channel the person is in, not only the open one', async () => {
    const { client } = shellWithChannels('/c/ch-q4');

    await waitFor(() => {
      expect(client.subscribed).toEqual(['ch-q4', 'ch-sales']);
    });
    expect(client.connects).toBe(1);
  });

  it('RemovedFromChannel_EventRemovesChannelFromRail', async () => {
    const { client } = shellWithChannels('/c/ch-q4');
    const rail = within(await screen.findByRole('navigation', RAIL));
    await rail.findByRole('link', { name: 'sales-th' });

    await act(async () => {
      client.emit(
        frame({
          case: 'membershipChanged',
          value: create(MembershipChangedSchema, { channelId: 'ch-sales', userId: ME_ID, change: MembershipChange.REMOVED }),
        }),
      );
      await Promise.resolve();
    });

    await waitFor(() => {
      expect(rail.queryByRole('link', { name: 'sales-th' })).not.toBeInTheDocument();
    });
    expect(rail.getByRole('link', { name: 'q4-launch' })).toBeInTheDocument();
  });

  it('loads the channel list again when the person is added to a channel', async () => {
    const { client, api } = shellWithChannels('/c/ch-q4');
    const rail = within(await screen.findByRole('navigation', RAIL));
    await rail.findByRole('link', { name: 'sales-th' });
    api.chat.listChannels.mockResolvedValue(
      create(ListChannelsResponseSchema, { channels: [...CHANNELS, channel({ id: 'ch-hr', name: 'hr-people', lastSeq: 0n })] }),
    );

    await act(async () => {
      client.emit(
        frame({
          case: 'membershipChanged',
          value: create(MembershipChangedSchema, { channelId: 'ch-hr', userId: ME_ID, change: MembershipChange.ADDED }),
        }),
      );
      await Promise.resolve();
    });

    expect(await rail.findByRole('link', { name: 'hr-people' })).toBeInTheDocument();
    await waitFor(() => {
      expect(client.subscribed).toContain('ch-hr');
    });
  });
});

describe('AccountMenu', () => {
  it('AccountMenu_SignOutLogsOutThenShowsSignedOutState', async () => {
    const order: string[] = [];
    const logout = vi.fn(() => {
      order.push('logout');
      return Promise.resolve(new Response(null, { status: 204 }));
    });
    vi.stubGlobal('fetch', logout);
    vi.spyOn(hardNavigate, 'to').mockImplementation((url) => {
      order.push(`navigate ${url}`);
    });
    const { user } = shellWithChannels('/c/ch-q4');
    await screen.findByRole('navigation', RAIL);

    await user.click(await screen.findByRole('button', { name: /Pimchanok S\./ }));

    const menu = await screen.findByRole('menu', { name: 'Account' });
    expect(menu).toHaveTextContent('Signed in as');
    expect(within(menu).getByText('pim@siamriverside.co.th')).toHaveClass('font-mono');
    const signOut = within(menu).getByRole('menuitem', { name: 'Sign out' });
    expect(signOut).toHaveFocus();

    await user.click(signOut);

    await waitFor(() => {
      expect(hardNavigate.to).toHaveBeenCalledWith('/signin?signedout=1');
    });
    expect(logout).toHaveBeenCalledWith('/api/auth/logout', expect.objectContaining({ method: 'POST' }));
    expect(order).toEqual(['logout', 'navigate /signin?signedout=1']);
  });

  it('stays signed in, and says so, when the server cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))));
    vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const { user } = shellWithChannels('/c/ch-q4');
    await screen.findByRole('navigation', RAIL);

    await user.click(await screen.findByRole('button', { name: /Pimchanok S\./ }));
    await user.click(within(await screen.findByRole('menu', { name: 'Account' })).getByRole('menuitem', { name: 'Sign out' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't sign out. Try again.");
    expect(hardNavigate.to).not.toHaveBeenCalled();
  });
});

describe('AccountSheet', () => {
  it('AccountSheet_MeTabOpensDialogWithPageInert', async () => {
    const { user } = shellWithChannels('/c/ch-q4');
    const tabs = within(await screen.findByRole('navigation', { name: 'Main navigation' }));
    const me = tabs.getByRole('button', { name: 'Me' });
    expect(me).toHaveAttribute('aria-expanded', 'false');
    await screen.findByRole('navigation', RAIL);

    await user.click(me);

    const sheet = await screen.findByRole('dialog', { name: 'Account' });
    expect(me).toHaveAttribute('aria-expanded', 'true');
    expect(me).toHaveAttribute('aria-controls', sheet.id);
    expect(within(sheet).getByText('Pimchanok S.')).toBeInTheDocument();
    expect(within(sheet).getByText('Siam Riverside Trading')).toBeInTheDocument();
    expect(within(sheet).getByText('pim@siamriverside.co.th')).toHaveClass('font-mono');
    expect(within(sheet).getByRole('button', { name: 'Close' })).toHaveFocus();
    // The page behind is out of reach: nothing outside the dialog is in the accessibility tree, and focus cannot leave it.
    expect(screen.queryByRole('navigation', RAIL)).not.toBeInTheDocument();
    expect(screen.queryByRole('navigation', { name: 'Main navigation' })).not.toBeInTheDocument();
    await user.tab();
    await user.tab();
    expect(sheet).toContainElement(document.activeElement as HTMLElement);

    await user.keyboard('{Escape}');

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    expect(me).toHaveFocus();
    expect(me).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByRole('navigation', RAIL)).toBeInTheDocument();
  });

  // Safari and Firefox on macOS do not focus a button on a click or a tap: focus must still return to the Me tab.
  it('returns focus to the Me tab when it was never focused', async () => {
    const { user } = shellWithChannels('/c/ch-q4');
    const tabs = within(await screen.findByRole('navigation', { name: 'Main navigation' }));
    const me = tabs.getByRole('button', { name: 'Me' });
    expect(document.body).toHaveFocus();

    fireEvent.click(me);
    await screen.findByRole('dialog', { name: 'Account' });
    await user.keyboard('{Escape}');

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: 'Me' })).toHaveFocus();
  });

  it('signs out from the sheet with the same logout, and shows the failure inside it', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(null, { status: 500 }))));
    vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const { user } = shellWithChannels('/c/ch-q4');
    const tabs = within(await screen.findByRole('navigation', { name: 'Main navigation' }));
    await waitFor(() => {
      expect(tabs.getByRole('button', { name: 'Me' })).toBeEnabled();
    });
    await user.click(tabs.getByRole('button', { name: 'Me' }));

    await user.click(within(await screen.findByRole('dialog', { name: 'Account' })).getByRole('button', { name: 'Sign out' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't sign out. Try again.");
    expect(hardNavigate.to).not.toHaveBeenCalled();
  });

  it('opens the Chat tab on the channel last open', async () => {
    useUiStore.getState().setLastChannel('ch-sales');
    shellWithChannels('/board');
    const tabs = within(await screen.findByRole('navigation', { name: 'Main navigation' }));

    await waitFor(() => {
      expect(tabs.getByRole('link', { name: 'Chat' })).toHaveAttribute('href', '/c/ch-sales');
    });
    expect(tabs.getByRole('link', { name: 'Board' })).toHaveAttribute('aria-current', 'page');
  });
});
