import type { I18nKey } from '@kiwi/core';

/** The `?error=` codes the server's callback redirects to `/signin` with. */
export const ERROR_CODES = ['state_mismatch', 'domain_not_allowed', 'not_invited', 'access_denied', 'exchange_failed'] as const;

export type SignInErrorNotice = {
  key: I18nKey;
  /** The address the server rejected, set in mono before the sentence. */
  email?: string;
};

/** The longest address there is (RFC 5321: 254 characters in a path). */
const MAX_EMAIL_LENGTH = 254;

/**
 * An address in shape: something, one `@`, something with a dot in it, and no space, control, format (zero-width, right-to-left
 * override) or other invisible character anywhere. It is a shape check, not validation of a mailbox: the server verified the
 * address, and the query that carries it is a link anyone can write.
 */
const EMAIL_SHAPE = /^[^\s@\p{C}]+@[^\s@\p{C}.]+(\.[^\s@\p{C}.]+)+$/u;

/**
 * The message for an `?error=` code, as a key of both language tables. Only `domain_not_allowed` and `not_invited` carry an
 * address, taken from the verified token by the server and shown only when it has the shape of an address and is at most 254
 * characters (a link can say anything in `email`; anything else gets the sentence without an address). `domain_not_allowed`
 * names the workspace when there is one to name, and `not_invited` (a Google account without a Workspace domain that nobody
 * added) always says "this workspace". A code this build does not know is answered like a failed exchange, so a failure is
 * never a silent redirect.
 */
export function signInErrorNotice(code: string, { email: claimed, workspaceName }: { email?: string; workspaceName?: string }): SignInErrorNotice {
  const email = claimed !== undefined && claimed.length <= MAX_EMAIL_LENGTH && EMAIL_SHAPE.test(claimed) ? claimed : undefined;
  switch (code) {
    case 'state_mismatch':
      return { key: 'signInErrorStateMismatch' };
    case 'domain_not_allowed':
      if (email === undefined) {
        return { key: 'signInErrorDomainNotAllowed' };
      }
      return { key: workspaceName === undefined || workspaceName === '' ? 'signInNotInThisWorkspace' : 'signInNotInWorkspace', email };
    case 'not_invited':
      return email === undefined ? { key: 'signInErrorNotInvited' } : { key: 'signInNotInvited', email };
    case 'access_denied':
      return { key: 'signInErrorAccessDenied' };
    default:
      return { key: 'signInErrorExchangeFailed' };
  }
}
