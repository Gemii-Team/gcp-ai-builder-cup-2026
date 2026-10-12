import { base64Decode } from '@bufbuild/protobuf/wire';
import { Code, ConnectError } from '@connectrpc/connect';
import { describe, expect, it } from 'vitest';
import { meetFailure } from './errors';

/** An error as the API answers it: the `details` of the Connect JSON error, each `value` decoded from base64. */
function answer(code: Code, message: string, details: { type: string; value: string }[]): ConnectError {
  const error = new ConnectError(message, code);
  error.details.push(...details.map((detail) => ({ type: detail.type, value: base64Decode(detail.value) })));
  return error;
}

// Captured from the server: `{"code":"failed_precondition","message":"meet unavailable","details":[{"type":"google.rpc.ErrorInfo",
// "value":"...","debug":{"reason":"MEET_UNAVAILABLE","domain":"kiwi","metadata":{"cause":"api_disabled"}}}]}`.
const NOT_ENABLED = 'ChBNRUVUX1VOQVZBSUxBQkxFEgRraXdpGhUKBWNhdXNlEgxhcGlfZGlzYWJsZWQ';

describe('meetFailure', () => {
  it('reads the reason and cause the API puts in the ErrorInfo of a failed Meet call', () => {
    const error = answer(Code.FailedPrecondition, 'meet unavailable', [{ type: 'google.rpc.ErrorInfo', value: NOT_ENABLED }]);

    expect(meetFailure(error)).toEqual({ reason: 'MEET_UNAVAILABLE', cause: 'api_disabled' });
  });

  it('reads MEET_ACCESS too', () => {
    // reason MEET_ACCESS, domain kiwi, cause access_denied
    const value = 'CgtNRUVUX0FDQ0VTUxIEa2l3aRoWCgVjYXVzZRINYWNjZXNzX2RlbmllZA';

    expect(meetFailure(answer(Code.FailedPrecondition, 'meet access refused', [{ type: 'google.rpc.ErrorInfo', value }]))).toEqual({
      reason: 'MEET_ACCESS',
      cause: 'access_denied',
    });
  });

  it('has no failure for the consent answer, for other details, for an answer without one, or for an error that is not an answer', () => {
    // reason CONSENT_REQUIRED, domain kiwi, url /api/auth/meet-consent
    const consent = 'ChBDT05TRU5UX1JFUVVJUkVEEgRraXdpGh0KA3VybBIWL2FwaS9hdXRoL21lZXQtY29uc2VudA';

    expect(meetFailure(answer(Code.FailedPrecondition, 'consent required', [{ type: 'google.rpc.ErrorInfo', value: consent }]))).toBeUndefined();
    expect(meetFailure(answer(Code.FailedPrecondition, 'x', [{ type: 'google.rpc.Other', value: NOT_ENABLED }]))).toBeUndefined();
    expect(meetFailure(new ConnectError('meet unavailable', Code.Unavailable))).toBeUndefined();
    expect(meetFailure(new Error('network'))).toBeUndefined();
    expect(meetFailure(undefined)).toBeUndefined();
  });

  it('skips an unreadable detail instead of failing', () => {
    const error = new ConnectError('meet unavailable', Code.Unavailable);
    error.details.push({ type: 'google.rpc.ErrorInfo', value: new Uint8Array([0xff, 0xff, 0xff]) });

    expect(meetFailure(error)).toBeUndefined();
  });
});
