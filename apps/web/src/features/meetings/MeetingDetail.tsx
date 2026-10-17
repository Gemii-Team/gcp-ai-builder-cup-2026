import '../shell/screen.css';
import './meetings.css';
import { Button, Icon, IconButton, LangRuns, Pending } from '@kiwi/ui';
import { useRouter } from '@tanstack/react-router';
import { useMemo } from 'react';
import { useChannels, useMeeting, useMembers, useRetrySummary } from '../../api/hooks';
import type { MeetingView, SummaryItemView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { LoadFailed } from '../shell/LoadFailed';
import { DraftTasksPanel } from './DraftTasksPanel';
import { statusBadge } from './meetingStatus';

/** The route `/meeting/$meetingId`: the meeting's AI summary and its draft tasks, Asana style. */
export function MeetingDetailScreen({ meetingId }: { meetingId: string }) {
  const { plain } = useI18n();
  const router = useRouter();
  const meeting = useMeeting(meetingId);

  return (
    <div className="mtg">
      <main className="main">
        <header className="chead">
          <IconButton
            label={plain('back')}
            className="back"
            onClick={() => {
              router.history.back();
            }}
          >
            <Icon name="back" />
          </IconButton>
          <div className="ttl">
            <h1>{meeting.data !== undefined ? <LangRuns text={meeting.data.title} /> : <T k="meetingsTitle" />}</h1>
            {meeting.data !== undefined && <p className="sub mtg-sub">{meeting.data.ref}</p>}
          </div>
          {meeting.data !== undefined && (
            <div className="acts">
              <span className="mtg-badge" data-tone={statusBadge(meeting.data.status).tone}>
                <T k={statusBadge(meeting.data.status).key} />
              </span>
            </div>
          )}
        </header>
        <div className="mtg-scroller">
          {meeting.isPending && <Pending label={plain('loading')} />}
          {meeting.isError && meeting.data === undefined && (
            <LoadFailed
              onRetry={() => {
                void meeting.refetch();
              }}
            >
              <T k="meetingLoadFailed" />
            </LoadFailed>
          )}
          {meeting.data !== undefined && <MeetingBody meeting={meeting.data} />}
        </div>
      </main>
    </div>
  );
}

function MeetingBody({ meeting }: { meeting: MeetingView }) {
  const { lang, plain } = useI18n();
  const channels = useChannels();
  const members = useMembers(meeting.channelId);
  const retry = useRetrySummary();

  const teamName = useMemo(() => (channels.data ?? []).find((channel) => channel.id === meeting.channelId)?.name ?? '', [channels.data, meeting.channelId]);
  const when = useMemo(() => formatWhen(meeting, lang), [meeting, lang]);

  return (
    <div className="mtg-detail">
      <div className="mtg-cols">
        <div className="mtg-col-main">
          <section className="mtg-card" aria-label={plain('meetingDetails')}>
            <h2 className="mtg-card-h">
              <T k="meetingDetails" />
            </h2>
            <dl className="mtg-facts">
              <div className="mtg-fact">
                <dt>
                  <Icon name="clock" size={16} />
                  <T k="meetingTime" />
                </dt>
                <dd>{when ?? <T k="meetingTimeUnknown" />}</dd>
              </div>
              <div className="mtg-fact">
                <dt>
                  <Icon name="user" size={16} />
                  <T k="meetingParticipants" />
                </dt>
                <dd className="mtg-people">
                  {teamName !== '' && <span className="mtg-team-chip">#{teamName}</span>}
                  {(members.data ?? []).map((member) => (
                    <span key={member.userId} className="mtg-person">
                      <LangRuns text={member.name} />
                    </span>
                  ))}
                </dd>
              </div>
            </dl>
            {meeting.meetUri !== '' && (
              <a className="mtg-join" href={meeting.meetUri} target="_blank" rel="noreferrer noopener">
                <Icon name="video" />
                <T k="eventJoin" />
              </a>
            )}
          </section>

          <section className="mtg-card" aria-label={plain('meetingSummaryHeading')}>
            <h2 className="mtg-card-h">
              <T k="meetingSummaryHeading" />
            </h2>
            <SummaryBody meeting={meeting} retrying={retry.isPending} onRetry={() => retry.mutate(meeting.id)} />
          </section>
        </div>

        <DraftTasksPanel meetingId={meeting.id} channelId={meeting.channelId} />
      </div>
    </div>
  );
}

function SummaryBody({ meeting, retrying, onRetry }: { meeting: MeetingView; retrying: boolean; onRetry: () => void }) {
  if (meeting.status === 'ready') {
    return (
      <div className="mtg-summary">
        {meeting.summary !== '' && (
          <div className="mtg-overview">
            <h3 className="mtg-sec-h">
              <T k="meetingObjective" />
            </h3>
            <p className="mtg-overview-body">
              <LangRuns text={meeting.summary} />
            </p>
          </div>
        )}
        <ItemSection titleKey="meetingDecisions" items={meeting.decisions} />
        <ItemSection titleKey="meetingOpenQuestions" items={meeting.openQuestions} />
        {meeting.summary === '' && meeting.decisions.length === 0 && meeting.openQuestions.length === 0 && (
          <p className="mtg-note">
            <T k="meetingNoSummaryYet" />
          </p>
        )}
      </div>
    );
  }
  if (meeting.status === 'failed') {
    return (
      <div className="mtg-state" data-tone="failed">
        <p>
          <T k="meetingFailedBody" params={{ reason: meeting.failureReason === '' ? '—' : meeting.failureReason }} />
        </p>
        <Button variant="primary" disabled={retrying} onClick={onRetry}>
          <Icon name="clock" />
          <T k={retrying ? 'meetingRetrying' : 'meetingRetry'} />
        </Button>
      </div>
    );
  }
  if (meeting.status === 'no_transcript') {
    return (
      <p className="mtg-note">
        <T k="meetingNoTranscriptBody" />
      </p>
    );
  }
  return (
    <p className="mtg-note mtg-processing">
      <T k="meetingProcessingBody" />
    </p>
  );
}

function ItemSection({ titleKey, items }: { titleKey: 'meetingDecisions' | 'meetingOpenQuestions'; items: SummaryItemView[] }) {
  if (items.length === 0) {
    return null;
  }
  return (
    <div className="mtg-sec">
      <h3 className="mtg-sec-h">
        <T k={titleKey} />
      </h3>
      <ul className="mtg-items">
        {items.map((item, index) => (
          <li key={index}>
            <LangRuns text={item.text} />
          </li>
        ))}
      </ul>
    </div>
  );
}

/** The meeting's time as a local range, or null when nothing was recorded. */
function formatWhen(meeting: MeetingView, lang: 'en' | 'th'): string | null {
  if (meeting.startedAt === null) {
    return null;
  }
  const locale = lang === 'th' ? 'th-TH-u-ca-gregory-nu-latn' : 'en-GB';
  const start = new Date(meeting.startedAt);
  if (meeting.endedAt !== null) {
    return new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false }).formatRange(start, new Date(meeting.endedAt));
  }
  return new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false }).format(start);
}
