import './shell.css';
import { useRouter } from '@tanstack/react-router';
import { T } from '../../i18n';
import { LoadFailed } from './LoadFailed';

/**
 * What the signed-in layout shows when the guard could not ask the server who is signed in (a failure that is not "nobody
 * is"): the message and Retry, which runs the guard again. Never an endless loader and never a blank shell.
 */
export function SessionError() {
  const router = useRouter();
  return (
    <main className="session-error">
      <LoadFailed
        onRetry={() => {
          void router.invalidate();
        }}
      >
        <T k="appLoadFailed" />
      </LoadFailed>
    </main>
  );
}
