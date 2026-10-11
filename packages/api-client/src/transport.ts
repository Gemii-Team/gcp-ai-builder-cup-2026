import type { Transport } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';

/**
 * createApiTransport returns the Connect transport for the K.I.W.I. API: JSON
 * over fetch with the session cookie sent on every call. The transport adds
 * the `Connect-Protocol-Version: 1` header the API requires (it is the CSRF
 * defence together with the origin check).
 */
export function createApiTransport(baseUrl: string): Transport {
  return createConnectTransport({
    baseUrl,
    useBinaryFormat: false,
    fetch: (input, init) => globalThis.fetch(input, { ...init, credentials: 'include' }),
  });
}
