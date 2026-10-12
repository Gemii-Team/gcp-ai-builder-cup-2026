import { Code } from '@connectrpc/connect';
import type { I18nKey } from '@kiwi/core';
import { useRouter, useRouterState } from '@tanstack/react-router';
import { useCallback, useEffect, useState } from 'react';
import { useApi } from '../../api/client';
import { hasCode, meetFailure } from '../../api/errors';
import { hardNavigate } from '../../navigation';

/** What a start-meeting request leaves for the person to read: an `alert` when it failed, a `status` when it worked but asks something of the host. */
export type StartNotice = { role: 'alert' | 'status'; key: I18nKey };

/** What each cause of a failed Meet call says; a cause that is not here, or a failure the API did not type, gets the generic message. */
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

/** `href` without the query parameter `name`, keeping the rest of the query and the fragment as they were. */
function withoutQueryParam(href: string, name: string): string {
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
    .filter((pair) => pair !== '' && pair.split('=')[0] !== name);
  return `${beforeHash.slice(0, queryAt)}${kept.length > 0 ? `?${kept.join('&')}` : ''}${hash}`;
}

/**
 * Starts a Meet meeting for the channel as the signed-in person. A `FailedPrecondition` the API does not name a Meet failure
 * for means Google has not been granted yet: the page leaves for the consent flow, which returns to this route. A started
 * meeting opens its Meet link in a new tab, and says so when Meet will not transcribe it by itself (the host has to turn
 * that on). Any other failure says why when the API told why, and is otherwise reported generically, never silently dropped.
 */
export function useStartMeeting(channelId: string) {
  const api = useApi();
  const router = useRouter();
  const here = useRouterState({ select: (state) => state.location.href });
  const [starting, setStarting] = useState(false);
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
        hardNavigate.open(meeting.meetUri);
      }
    } catch (error) {
      setStarting(false);
      const failure = meetFailure(error);
      if (failure === undefined && hasCode(error, Code.FailedPrecondition)) {
        hardNavigate.to(`/api/auth/meet-consent?return_to=${encodeURIComponent(here)}`);
      } else {
        setNotice({ role: 'alert', key: (failure && MEET_FAILURE_KEY[failure.cause]) ?? 'startMeetingFailed' });
      }
    }
  }, [api, channelId, here]);

  return { start, starting, notice };
}
