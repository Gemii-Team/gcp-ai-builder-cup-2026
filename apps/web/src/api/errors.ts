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
