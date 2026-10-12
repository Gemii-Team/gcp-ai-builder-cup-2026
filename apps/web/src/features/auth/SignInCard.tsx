import { Icon, Pending, Perforation, Slip } from '@kiwi/ui';
import { useQuery } from '@tanstack/react-query';
import { useEffect, useId, useState, type ReactNode } from 'react';
import { T, TSlots, UserText } from '../../i18n';
import { hardNavigate } from '../../navigation';
import { authConfigQuery, EMPTY_AUTH_CONFIG } from './authConfig';
import { signInErrorNotice } from './errors';
import { GoogleButton } from './GoogleButton';
import { loginHref } from './returnTo';

const MONO = 'font-mono text-numeral font-semibold [overflow-wrap:anywhere]';

/** Something counted, referenced or addressed: Noto Sans Mono at 600, never split by the Thai or the English around it. */
function Mono({ children, ink = false }: { children: ReactNode; /** Ledger Ink, where the surrounding text is not. */ ink?: boolean }) {
  return (
    <span lang="en" className={ink ? `${MONO} text-ledger-ink` : MONO}>
      {children}
    </span>
  );
}

const WHAT_IT_SEES = ['signInSeeChannels', 'signInSeeFiles', 'signInSeeMeet'] as const;

/**
 * The slip that signs a person in: the headline (the workspace's name from `/api/auth/config`), the Google button, the
 * domain hint, and under the perforation what K.I.W.I. will see. `error` is a `?error=` code of the server's callback and
 * `email` the address it rejected; `signedOut` is the notice after sign-out; `returnTo` is the page to come back to, already
 * checked to be a path of this app.
 */
export function SignInCard({ returnTo, error, email, signedOut }: { returnTo: string; error?: string; email?: string; signedOut: boolean }) {
  const { workspaceName, domain } = useQuery(authConfigQuery).data ?? EMPTY_AUTH_CONFIG;
  const [redirecting, setRedirecting] = useState(false);
  const headingId = useId();
  const notice = error === undefined ? undefined : signInErrorNotice(error, { email, workspaceName });

  // The browser may show this page again from its back-forward cache, frozen at "Redirecting…": the person is back.
  useEffect(() => {
    const restore = (event: PageTransitionEvent) => {
      if (event.persisted) {
        setRedirecting(false);
      }
    };
    window.addEventListener('pageshow', restore);
    return () => {
      window.removeEventListener('pageshow', restore);
    };
  }, []);

  function signIn() {
    setRedirecting(true);
    hardNavigate.to(loginHref(returnTo));
  }

  return (
    <Slip role="region" aria-labelledby={headingId} className="si-slip">
      <div className="si-body">
        {signedOut && (
          <p className="si-out" role="status">
            <Icon name="check" />
            <T k="signInSignedOut" />
          </p>
        )}
        <h1 id={headingId} className="si-title">
          {workspaceName === '' ? <T k="signInTitleDefault" /> : <TSlots k="signInTitle" slots={{ workspace: <UserText text={workspaceName} /> }} />}
        </h1>
        {notice !== undefined && (
          <div className="si-fail" role="alert">
            <Icon name="warning" />
            <p>
              {notice.email !== undefined && (
                <>
                  <Mono>{notice.email}</Mono>{' '}
                </>
              )}
              <TSlots k={notice.key} slots={{ workspace: <UserText text={workspaceName} /> }} />
            </p>
          </div>
        )}
        <GoogleButton loading={redirecting} onClick={signIn} />
        {redirecting && <Pending label={<T k="signInRedirecting" />} />}
        {domain !== '' && (
          <p className="si-hint">
            <TSlots k="signInHint" slots={{ domain: <Mono ink>{domain.startsWith('@') ? domain : `@${domain}`}</Mono> }} />
          </p>
        )}
      </div>
      <Perforation />
      <div className="si-stub">
        <h2>
          <T k="signInSeeHeading" />
        </h2>
        <ul>
          {WHAT_IT_SEES.map((key) => (
            <li key={key}>
              <span className="si-bullet" aria-hidden="true" />
              <span>
                <T k={key} />
              </span>
            </li>
          ))}
          <li className="si-ask">
            <span className="si-bullet" aria-hidden="true" />
            <span>
              <T k="signInSeeAsks" />
            </span>
          </li>
        </ul>
      </div>
    </Slip>
  );
}
