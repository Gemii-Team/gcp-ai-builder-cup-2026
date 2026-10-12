import { Code } from '@connectrpc/connect';
import type { I18nKey } from '@kiwi/core';
import { useRouterState } from '@tanstack/react-router';
import { useCallback, useState } from 'react';
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

/**
 * Starts a Meet meeting for the channel as the signed-in person. A `FailedPrecondition` the API does not name a Meet failure
 * for means Google has not been granted yet: the page leaves for the consent flow, which returns to this route. A started
 * meeting opens its Meet link in a new tab, and says so when Meet will not transcribe it by itself (the host has to turn
 * that on). Any other failure says why when the API told why, and is otherwise reported generically, never silently dropped.
 */
export function useStartMeeting(channelId: string) {
  const api = useApi();
  const here = useRouterState({ select: (state) => state.location.href });
  const [starting, setStarting] = useState(false);
  const [notice, setNotice] = useState<StartNotice | null>(null);

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
