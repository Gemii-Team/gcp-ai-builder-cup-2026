import { createClient } from '@connectrpc/connect';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createApiTransport, IdentityService } from './index';

type Sent = { url: string; headers: Headers; credentials: RequestCredentials | undefined; body: string };

function bodyText(body: BodyInit | null | undefined): string {
  if (typeof body === 'string') {
    return body;
  }
  if (body instanceof Uint8Array) {
    return new TextDecoder().decode(body);
  }
  throw new Error('The request has no JSON or byte body.');
}

/**
 * Makes one call of every IdentityService method through the transport, with `fetch` stubbed (no network), and
 * returns what reached `fetch` for each. The stub answers an empty message in the format the request used, so a
 * transport that switched format still completes its calls and fails on the assertion about the format.
 */
async function callEveryMethod(): Promise<Sent[]> {
  const fetchStub = vi.fn<typeof fetch>((_, init) => {
    const binary = new Headers(init?.headers).get('Content-Type') === 'application/proto';
    return Promise.resolve(
      binary
        ? new Response(new Uint8Array(), { headers: { 'Content-Type': 'application/proto' } })
        : new Response('{}', { headers: { 'Content-Type': 'application/json' } }),
    );
  });
  vi.stubGlobal('fetch', fetchStub);
  const client = createClient(IdentityService, createApiTransport('http://x'));

  await client.getMe({});
  await client.listUsers({});
  await client.issueRealtimeTicket({});

  expect(fetchStub).toHaveBeenCalledTimes(3);
  return fetchStub.mock.calls.map(([input, init]) => ({
    url: String(input),
    headers: new Headers(init?.headers),
    credentials: init?.credentials,
    body: bodyText(init?.body),
  }));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

// The API's CSRF defence is the pair of Connect-Protocol-Version and a JSON or protobuf content type, which a
// cross-site "simple" request cannot carry, together with the session cookie that only credentials: 'include' sends.
describe('createApiTransport', () => {
  it('posts to the Connect path of the service under the base URL', async () => {
    const sent = await callEveryMethod();

    expect(sent.map(({ url }) => url)).toEqual([
      'http://x/kiwi.identity.v1.IdentityService/GetMe',
      'http://x/kiwi.identity.v1.IdentityService/ListUsers',
      'http://x/kiwi.identity.v1.IdentityService/IssueRealtimeTicket',
    ]);
  });

  it('sends JSON, not binary protobuf, with content-type application/json', async () => {
    for (const { headers, body } of await callEveryMethod()) {
      expect(headers.get('Content-Type')).toBe('application/json');
      expect(JSON.parse(body)).toEqual({});
    }
  });

  it('sends Connect-Protocol-Version: 1 on every request', async () => {
    for (const { headers } of await callEveryMethod()) {
      expect(headers.get('Connect-Protocol-Version')).toBe('1');
    }
  });

  it("sends the session cookie: credentials 'include' on every request", async () => {
    for (const { credentials } of await callEveryMethod()) {
      expect(credentials).toBe('include');
    }
  });
});
