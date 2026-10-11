import { Code } from '@connectrpc/connect';
import { useRouterState } from '@tanstack/react-router';
import { useCallback, useState } from 'react';
import { useApi } from '../../api/client';
import { hasCode } from '../../api/errors';
import { hardNavigate } from '../../navigation';

/**
 * Starts a Meet meeting for the channel as the signed-in person. A request the server answers with `FailedPrecondition`
 * means Google has not been granted yet: the page leaves for the consent flow, which returns to this route. A started
 * meeting opens its Meet link in a new tab; any other failure is reported, never silently dropped.
 */
export function useStartMeeting(channelId: string) {
  const api = useApi();
  const here = useRouterState({ select: (state) => state.location.href });
  const [state, setState] = useState<'idle' | 'starting' | 'failed'>('idle');

  const start = useCallback(async () => {
    setState('starting');
    try {
      const { meeting } = await api.meetings.startMeeting({ channelId });
      setState('idle');
      if (meeting !== undefined && meeting.meetUri !== '') {
        hardNavigate.open(meeting.meetUri);
      }
    } catch (error) {
      if (hasCode(error, Code.FailedPrecondition)) {
        setState('idle');
        hardNavigate.to(`/api/auth/meet-consent?return_to=${encodeURIComponent(here)}`);
      } else {
        setState('failed');
      }
    }
  }, [api, channelId, here]);

  return { start, starting: state === 'starting', failed: state === 'failed' };
}
