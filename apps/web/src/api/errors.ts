import { BinaryReader } from '@bufbuild/protobuf/wire';
import { Code, ConnectError } from '@connectrpc/connect';

/**
 * What a failed `SendMessage` means for the message. `duplicate`: the server already stored it (same `client_msg_id`),
 * which is as good as sent. `rejected`: the server answered no, so resending the same message cannot help. `retry`:
 * no answer came (the network, a gateway, a timeout), so it waits for the connection and goes again.
 */
export function sendOutcome(error: unknown): 'duplicate' | 'rejected' | 'retry' {
  if (!(error instanceof ConnectError)) {
    return 'retry';
  }
  switch (error.code) {
    case Code.AlreadyExists:
      return 'duplicate';
    case Code.InvalidArgument:
    case Code.NotFound:
    case Code.PermissionDenied:
    case Code.FailedPrecondition:
    case Code.Unauthenticated:
    case Code.OutOfRange:
    case Code.Unimplemented:
      return 'rejected';
    default:
      return 'retry';
  }
}

/** Whether the server answered with this status code (`FailedPrecondition` is how a start-meeting request asks for consent). */
export function hasCode(error: unknown, code: Code): boolean {
  return error instanceof ConnectError && error.code === code;
}

/**
 * What the API says a failed Meet call was, read off the `google.rpc.ErrorInfo` detail of its answer: `MEET_ACCESS` (Google
 * refuses the person's access to Meet) or `MEET_UNAVAILABLE` (Meet cannot do the call), and the `cause` that tells which
 * way (`access_denied`, `api_disabled`, `rate_limited`, `unavailable`, `refused`). `undefined` for any other error,
 * including the consent answer, whose ErrorInfo has another reason.
 */
export type MeetFailure = { reason: 'MEET_ACCESS' | 'MEET_UNAVAILABLE'; cause: string };

export function meetFailure(error: unknown): MeetFailure | undefined {
  if (!(error instanceof ConnectError)) {
    return undefined;
  }
  for (const detail of error.details) {
    if ('type' in detail && detail.type === 'google.rpc.ErrorInfo') {
      const info = readErrorInfo(detail.value);
      if (info?.reason === 'MEET_ACCESS' || info?.reason === 'MEET_UNAVAILABLE') {
        return { reason: info.reason, cause: info.metadata.get('cause') ?? '' };
      }
    }
  }
  return undefined;
}

/** `reason` (field 1) and `metadata` (field 3, a map) of a `google.rpc.ErrorInfo`; `undefined` when the bytes are not one. */
function readErrorInfo(bytes: Uint8Array): { reason: string; metadata: Map<string, string> } | undefined {
  try {
    const info = { reason: '', metadata: new Map<string, string>() };
    for (const reader = new BinaryReader(bytes); reader.pos < reader.len; ) {
      const [field, wireType] = reader.tag();
      if (field === 1) {
        info.reason = reader.string();
      } else if (field === 3) {
        const entry = new BinaryReader(reader.bytes());
        let key = '';
        let value = '';
        while (entry.pos < entry.len) {
          const [entryField, entryType] = entry.tag();
          if (entryField === 1) {
            key = entry.string();
          } else if (entryField === 2) {
            value = entry.string();
          } else {
            entry.skip(entryType);
          }
        }
        info.metadata.set(key, value);
      } else {
        reader.skip(wireType);
      }
    }
    return info;
  } catch {
    return undefined;
  }
}

