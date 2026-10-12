// The channel screen mounted the way the route `/c/$channelId` mounts it. Test support: nothing in the app imports this file.
import type { SendMessageRequest } from '@kiwi/api-client';
import { ChannelScreen } from '../features/channel/ChannelScreen';
import { channel } from './fixtures';
import { renderApp } from './render';
import { createStubApi, seedApi, type StubApi, type World } from './stubApi';

/** The stream of `#q4-launch`, by its accessible name. */
export const LOG = { name: 'Messages in #q4-launch' };

/** `#q4-launch` in the shell, at `path` (a query a redirect brought back included). `messages` are newest first, as the server sends them. */
export function openChannel(world: World = {}, api: StubApi = createStubApi(), path = '/c/ch-q4') {
  seedApi(api, { channels: [channel({ id: 'ch-q4', name: 'q4-launch', lastSeq: 5n })], ...world });
  return renderApp({ path, api, routes: [{ path: '/c/$channelId', element: <ChannelScreen channelId="ch-q4" /> }] });
}

/** The request of the `call`th `SendMessage`. */
export function sentRequest(api: StubApi, call = 0): SendMessageRequest {
  const request = api.chat.sendMessage.mock.calls[call]?.[0];
  if (request === undefined) {
    throw new Error('SendMessage was not called');
  }
  return request as SendMessageRequest;
}
