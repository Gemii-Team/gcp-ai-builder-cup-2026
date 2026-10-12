import { Code } from '@connectrpc/connect';
import type { I18nKey } from '@kiwi/core';
import { useRouter, useRouterState } from '@tanstack/react-router';
import { useCallback, useEffect, useState } from 'react';
import { useApi } from '../../api/client';
import { hasCode, meetFailure } from '../../api/errors';
import { useMe } from '../../api/hooks';
import { hardNavigate } from '../../navigation';

/** What a start-meeting request leaves for the person to read: an `alert` when it failed, a `status` when it worked but asks something of the host. */
export type StartNotice = { role: 'alert' | 'status'; key: I18nKey };

/**
 * What each cause of a failed Meet call says; a cause that is not here, or a failure the API did not type, gets the generic
 * message. The cause comes from the server, so it is looked up with `Object.hasOwn`: it can never name an inherited property.
 */
const MEET_FAILURE_KEY: Record<string, I18nKey> = {
  access_denied: 'startMeetingMeetAccess',
  api_disabled: 'startMeetingApiDisabled',
  rate_limited: 'startMeetingRateLimited',
  unavailable: 'startMeetingMeetUnavailable',
  refused: 'startMeetingRefused',
};

/** The query parameter the consent flow adds to the address it returns to when it ended without a Meet grant. */
const CONSENT_MISSING_PARAM = 'meet_consent';
const CONSENT_MISSING_VALUE = 'missing';

/**
 * The Meet link for the host's own tab: `uri` with `authuser=<email>` added, so a browser signed in to several Google
 * accounts opens Meet as the one that created the space (otherwise Meet picks its default account, which is not the host and
 * asks to join). `authuser` is the parameter Google's own pages use to pick an account; it is not documented for Meet, so
 * this leans on behaviour Google could change, and the worst it can do is what happened before: Meet opens as another
 * account. The link is changed only when it is an https link on exactly meet.google.com (no credentials, no port) that does
 * not already choose an account; anything else is returned as it is. The link the server stored and posted in the channel
 * stays clean, since other people must not get the host's account in theirs.
 */
export function withAuthUser(uri: string, email: string): string {
  if (email === '' || !URL.canParse(uri)) {
    return uri;
  }
  const url = new URL(uri);
  if (url.protocol !== 'https:' || url.host !== 'meet.google.com' || url.username !== '' || url.password !== '' || url.searchParams.has('authuser')) {
    return uri;
  }
  const hashAt = uri.indexOf('#');
  const beforeHash = hashAt === -1 ? uri : uri.slice(0, hashAt);
  const hash = hashAt === -1 ? '' : uri.slice(hashAt);
  const join = !beforeHash.includes('?') ? '?' : /[?&]$/.test(beforeHash) ? '' : '&';
  return `${beforeHash}${join}authuser=${encodeURIComponent(email)}${hash}`;
}

/**
 * `href` without the query parameter `name`, keeping the rest of the query and the fragment as they were. Names are
 * compared decoded, so `meet%5Fconsent=missing` is the parameter `meet_consent` too.
 */
export function withoutQueryParam(href: string, name: string): string {
  const hashAt = href.indexOf('#');
  const hash = hashAt === -1 ? '' : href.slice(hashAt);
  const beforeHash = hashAt === -1 ? href : href.slice(0, hashAt);
  const queryAt = beforeHash.indexOf('?');
  if (queryAt === -1) {
    return href;
  }
  const kept = beforeHash
    .slice(queryAt + 1)
    .split('&')
    .filter((pair) => pair !== '' && !new URLSearchParams(pair).has(name));
  return `${beforeHash.slice(0, queryAt)}${kept.length > 0 ? `?${kept.join('&')}` : ''}${hash}`;
}

/**
 * Starts a Meet meeting for the channel as the signed-in person. A `FailedPrecondition` the API does not name a Meet failure
 * for means Google has not been granted yet: the page leaves for the consent flow, which returns to this route. A started
 * meeting opens its Meet link in a new tab (a link that is not https is not opened, and the generic failure is shown), and
 * says so when Meet will not transcribe it by itself (the host has to turn that on). Any other failure says why when the API
 * told why, and is otherwise reported generically, never silently dropped.
 */
export function useStartMeeting(channelId: string) {
  const api = useApi();
  const router = useRouter();
  const here = useRouterState({ select: (state) => state.location.href });
  const [starting, setStarting] = useState(false);
  const me = useMe();
  const email = me.data?.user.email ?? '';
  // A consent round trip that stored no grant comes back here with the flag: say so once, then take the flag off the address.
  const consentMissing = useRouterState({ select: (state) => new URLSearchParams(state.location.searchStr).get(CONSENT_MISSING_PARAM) === CONSENT_MISSING_VALUE });
  const [notice, setNotice] = useState<StartNotice | null>(consentMissing ? { role: 'alert', key: 'startMeetingConsentMissing' } : null);

  useEffect(() => {
    if (consentMissing) {
      router.history.replace(withoutQueryParam(router.state.location.href, CONSENT_MISSING_PARAM));
    }
  }, [consentMissing, router]);

  const start = useCallback(async () => {
    setStarting(true);
    setNotice(null);
    try {
      const { meeting, transcriptionAutomatic } = await api.meetings.startMeeting({ channelId });
      setStarting(false);
      if (!transcriptionAutomatic) {
        setNotice({ role: 'status', key: 'startMeetingManualTranscription' });
      }
      if (meeting !== undefined && meeting.meetUri !== '') {
        // The link comes from the server and goes to `window.open`: only an https link is ever opened (`javascript:`, `data:` and `http:` are not).
        if (URL.canParse(meeting.meetUri) && new URL(meeting.meetUri).protocol === 'https:') {
          hardNavigate.open(withAuthUser(meeting.meetUri, email));
        } else {
          setNotice({ role: 'alert', key: 'startMeetingFailed' });
        }
      }
    } catch (error) {
      setStarting(false);
      const failure = meetFailure(error);
      if (failure === undefined && hasCode(error, Code.FailedPrecondition)) {
        hardNavigate.to(`/api/auth/meet-consent?return_to=${encodeURIComponent(here)}`);
      } else {
        setNotice({ role: 'alert', key: failure && Object.hasOwn(MEET_FAILURE_KEY, failure.cause) ? MEET_FAILURE_KEY[failure.cause] : 'startMeetingFailed' });
      }
    }
  }, [api, channelId, here, email]);

  return { start, starting, notice };
}
