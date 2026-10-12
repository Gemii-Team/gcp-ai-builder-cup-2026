import { formatClock, formatDay, Icon, LangRuns, Numeral, RefCode, type IconName } from '@kiwi/ui';
import { useQuery } from '@tanstack/react-query';
import { useId, type ReactNode } from 'react';
import { useApi } from '../../api/client';
import { fileQuery, meetingQuery } from '../../api/queries';
import type { FileView, MeetingView, TaskView } from '../../api/types';
import { T, TCount, TSlots, useI18n } from '../../i18n';
import { Cell } from './Cell';
import type { Lane as LaneData, LaneKind, Status } from './useBoardData';
import { STATUSES } from './useBoardData';

const KIND_ICON: Record<LaneKind, IconName> = { hand: 'pen', meeting: 'video', file: 'file', channel: 'chat' };

/** The lane's title: the source's own name, `Messages in #channel` for a channel's messages, Added by hand for none. */
function LaneTitle({ lane }: { lane: LaneData }) {
  switch (lane.kind) {
    case 'hand':
      return <T k="sourceByHand" />;
    case 'channel':
      return <T k="messagesIn" params={{ channel: lane.label ?? '' }} />;
    default:
      return <LangRuns text={lane.label ?? ''} />;
  }
}

/** `Meeting · Tue 13 Oct · ended 14:29`: what a meeting's own record says, or just `Meeting` until it is known. */
function MeetingMeta({ meeting }: { meeting: MeetingView | undefined }) {
  const { lang } = useI18n();
  const started = meeting?.startedAt ?? meeting?.endedAt ?? null;
  if (started === null) {
    return <T k="laneMeetingMetaUndated" />;
  }
  const day = formatDay(started, lang);
  const ended = meeting?.endedAt ?? null;
  return ended === null ? (
    <T k="laneMeetingMetaOpen" params={{ day }} />
  ) : (
    <TSlots k="laneMeetingMeta" params={{ day }} slots={{ time: <Numeral>{formatClock(ended)}</Numeral> }} />
  );
}

/** `Document · 18 pages · #sales-th`; the page count is left out when the file has none. */
function FileMeta({ file, channelName }: { file: FileView | undefined; channelName: string }) {
  return file === undefined || file.pages === 0 ? (
    <T k="laneDocumentMetaNoPages" params={{ channel: channelName }} />
  ) : (
    <TCount k="laneDocumentMeta" count={file.pages} params={{ channel: channelName }} />
  );
}

type LaneProps = {
  lane: LaneData;
  channelName: string;
  /** On phones a lane shows one status; wide, all three. */
  only: Status | null;
  renderCard: (task: TaskView, lane: LaneData) => ReactNode;
};

/**
 * One source and the three status cells its tasks move between. A plain heading names the source, with its REF when it
 * has one and its meta line (a meeting's day and end, a document's page count), read through `GetMeeting` and `GetFile`
 * under the same cache keys the excerpts use.
 */
export function Lane({ lane, channelName, only, renderCard }: LaneProps) {
  const api = useApi();
  const { plain } = useI18n();
  const headingId = useId();
  const titleText = lane.kind === 'hand' ? plain('sourceByHand') : lane.kind === 'channel' ? plain('messagesIn', { channel: lane.label ?? '' }) : (lane.label ?? '');
  const sourceId = lane.sourceId ?? '';
  const meeting = useQuery({ ...meetingQuery(api, sourceId), enabled: lane.kind === 'meeting' && sourceId !== '' }).data;
  const file = useQuery({ ...fileQuery(api, sourceId), enabled: lane.kind === 'file' && sourceId !== '' }).data;

  let meta: ReactNode;
  let ref: string | undefined;
  switch (lane.kind) {
    case 'hand':
      meta = <T k="laneAddedByHandMeta" />;
      break;
    case 'channel':
      meta = <T k="laneMessagesMeta" />;
      break;
    case 'meeting':
      meta = <MeetingMeta meeting={meeting} />;
      ref = meeting?.ref;
      break;
    case 'file':
      meta = <FileMeta file={file} channelName={channelName} />;
      ref = file?.ref;
      break;
  }

  return (
    <section className="bd-lane" aria-labelledby={headingId}>
      <h2 className="bd-lane-h" id={headingId}>
        <Icon name={KIND_ICON[lane.kind]} className="bd-kind" />
        <span className="bd-ln">
          <span className="bd-lt">
            <LaneTitle lane={lane} />
          </span>
          <span className="bd-lm">{meta}</span>
        </span>
        {ref !== undefined && ref !== '' && <RefCode code={ref} />}
        <span className="bd-lcount">
          <TCount k="laneTaskCount" count={lane.total} />
        </span>
      </h2>
      <div className="bd-lane-b">
        {STATUSES.filter((status) => only === null || status === only).map((status) => (
          <Cell key={status} status={status} laneTitle={titleText} tasks={lane.cells[status]} renderCard={(task) => renderCard(task, lane)} />
        ))}
      </div>
    </section>
  );
}
