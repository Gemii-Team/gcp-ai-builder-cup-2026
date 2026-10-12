import { queryOptions } from '@tanstack/react-query';
import { keys } from '../../api/queries';

/** What `GET /api/auth/config` answers: the names the sign-in page shows. Presentational only; the server authorises. */
export type AuthConfig = { workspaceName: string; domain: string };

export const EMPTY_AUTH_CONFIG: AuthConfig = { workspaceName: '', domain: '' };

function textField(body: object, name: string): string {
  const value: unknown = Reflect.get(body, name);
  return typeof value === 'string' ? value : '';
}

/**
 * Reads `/api/auth/config` on the page's own origin. It is a plain `fetch` because the endpoint is not part of the
 * generated client. It never throws: a server that cannot answer leaves the names empty, and sign-in still works.
 */
export async function fetchAuthConfig(): Promise<AuthConfig> {
  try {
    const response = await fetch('/api/auth/config', { credentials: 'same-origin' });
    if (!response.ok) {
      return EMPTY_AUTH_CONFIG;
    }
    const body: unknown = await response.json();
    if (typeof body !== 'object' || body === null) {
      return EMPTY_AUTH_CONFIG;
    }
    return { workspaceName: textField(body, 'workspaceName'), domain: textField(body, 'domain') };
  } catch {
    return EMPTY_AUTH_CONFIG;
  }
}

/** The names do not change while the page is open. */
export const authConfigQuery = queryOptions({ queryKey: keys.authConfig(), queryFn: fetchAuthConfig, staleTime: Infinity });
