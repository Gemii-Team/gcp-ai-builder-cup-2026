import '../shell/screen.css';
import './meetings.css';
import { Icon, LangRuns, Pending } from '@kiwi/ui';
import { useMemo } from 'react';
import { useChannels, useMeetings } from '../../api/hooks';
import type { MeetingView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { AppLink } from '../shell/AppLink';
import { LoadFailed } from '../shell/LoadFailed';
import { statusBadge } from './meetingStatus';

/** The route `/meetings`: every meeting of the person's channels, newest first, each opening its detail. */
export function MeetingsListScreen() {
  const { lang, plain } = useI18n();
  const meetings = useMeetings();
  const channels = useChannels();
  const channelNames = useMemo(() => new Map((channels.data ?? []).map((channel) => [channel.id, channel.name])), [channels.data]);
  const when = useMemo(
    () => new Intl.DateTimeFormat(lang === 'th' ? 'th-TH-u-ca-gregory-nu-latn' : 'en-GB', { day: 'numeric', month: 'short', year: 'numeric' }),
    [lang],
  );

  return (
    <div className="mtg">
      <main className="main">
        <header className="chead">
          <div className="ttl">
            <h1>
              <T k="meetingsTitle" />
            </h1>
          </div>
        </header>
        <div className="mtg-scroller">
          {meetings.isPending && <Pending label={plain('loading')} />}
          {meetings.isError && meetings.data === undefined && (
            <LoadFailed
              onRetry={() => {
                void meetings.refetch();
              }}
            >
              <T k="meetingsLoadFailed" />
            </LoadFailed>
          )}
          {meetings.data !== undefined && meetings.data.length === 0 && (
            <p className="mtg-empty">
              <T k="meetingsEmpty" />
            </p>
          )}
          {meetings.data !== undefined && meetings.data.length > 0 && (
            <ul className="mtg-list">
              {meetings.data.map((meeting) => (
                <li key={meeting.id}>
                  <MeetingRow meeting={meeting} teamName={channelNames.get(meeting.channelId) ?? ''} when={meeting.startedAt !== null ? when.format(new Date(meeting.startedAt)) : null} openLabel={plain('meetingOpen')} />
                </li>
              ))}
            </ul>
          )}
        </div>
      </main>
    </div>
  );
}

function MeetingRow({ meeting, teamName, when, openLabel }: { meeting: MeetingView; teamName: string; when: string | null; openLabel: string }) {
  const badge = statusBadge(meeting.status);
  return (
    <AppLink to={`/meeting/${meeting.id}`} className="mtg-row" aria-label={`${openLabel}: ${meeting.title}`}>
      <span className="mtg-row-ic" aria-hidden="true">
        <Icon name="video" />
      </span>
      <span className="mtg-row-main">
        <span className="mtg-row-ttl">
          <LangRuns text={meeting.title} />
        </span>
        <span className="mtg-row-meta">
          <span className="mtg-ref">{meeting.ref}</span>
          {teamName !== '' && <span className="mtg-team">#{teamName}</span>}
          {when !== null && <span className="mtg-when">{when}</span>}
        </span>
      </span>
      <span className="mtg-badge" data-tone={badge.tone}>
        <T k={badge.key} />
      </span>
    </AppLink>
  );
}
