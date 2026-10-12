import { FailedSlip, FileChip, formatClock, Icon, LangRuns, PendingSlip } from '@kiwi/ui';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { useApi } from '../../api/client';
import { useFile } from '../../api/hooks';
import { keys, meetingQuery } from '../../api/queries';
import type { ChatMessage, FileRefView, FileView, MeetingView, RecordingUpload } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { failureReasonKey } from '../../lib/failure';
import { initialOf } from '../../lib/text';
import { Sentence } from './Sentence';

type SlipProps = { file: FileRefView; message: ChatMessage };

type FailureProps = {
  reason: ReactNode;
  failedAt: string;
  /** Asks the server to try again; resolves once it has agreed. */
  retry: () => Promise<void>;
};

/**
 * A failed slip with Retry. When the request itself fails (the service is down, which is when people press Retry most)
 * the slip stays failed, a calm notice says so and Retry can be pressed again; nothing is left unhandled.
 */
function Failure({ reason, failedAt, retry }: FailureProps) {
  const [failed, setFailed] = useState(false);
  return (
    <div className="msg-b">
      <FailedSlip
        reason={reason}
        failedAt={failedAt}
        onRetry={() => {
          setFailed(false);
          retry().catch(() => {
            setFailed(true);
          });
        }}
      />
      {failed && (
        <p className="notice" role="alert">
          <Icon name="warning" />
          <T k="retryFailed" />
        </p>
      )}
    </div>
  );
}

/** An attached file under its message: a calm wait while it is summarized, an honest failure with Retry. A ready file has its summary as a message of its own. */
function FileSlip({ file, message }: SlipProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { plain } = useI18n();
  const status = useFile(file.fileId).data;

  if (status === undefined || status.status === 'ready') {
    return null;
  }
  if (status.status === 'failed') {
    return (
      <Failure
        reason={<Sentence k="fileSummaryFailed" text={file.name} reason={failureReasonKey(status.failureReason)} />}
        failedAt={status.settledAt ?? message.createdAt}
        retry={async () => {
          await api.files.retryAnalysis({ fileId: file.fileId });
          queryClient.setQueryData<FileView>(keys.file(file.fileId), (current) => current && { ...current, status: 'analyzing', failureReason: '' });
        }}
      />
    );
  }
  return (
    <div className="msg-b">
      <PendingSlip label={plain('summarizingFile')} />
    </div>
  );
}

/** The status slips of the files a message carries, derived from the file cache that realtime events keep current. */
export function MessageFiles({ message }: { message: ChatMessage }) {
  return message.files.map((file) => <FileSlip key={file.fileId} file={file} message={message} />);
}

/**
 * A recording this session uploaded, as a row of its own: the file's chip, then the meeting it became while that is being
 * summarized (a calm wait) or could not be (failed, with Retry). The server posts no message for a recording, so there is
 * nothing of the stream to hang it under; once the summary is posted as its own message the row is gone.
 */
export function RecordingStatus({ recording, authorName }: { recording: RecordingUpload; authorName: string }) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { plain } = useI18n();
  const meeting = useQuery(meetingQuery(api, recording.meetingId)).data;

  if (meeting === undefined || meeting.status === 'ready') {
    return null;
  }
  const slip =
    meeting.status === 'failed' || meeting.status === 'no_transcript' ? (
      <Failure
        reason={<Sentence k="meetingSummaryFailed" slot="title" text={meeting.title} reason={failureReasonKey(meeting.failureReason)} />}
        failedAt={meeting.settledAt ?? recording.at}
        retry={async () => {
          await api.meetings.retrySummary({ meetingId: meeting.id });
          queryClient.setQueryData<MeetingView>(keys.meeting(meeting.id), (current) => current && { ...current, status: 'ingesting', failureReason: '' });
        }}
      />
    ) : (
      <div className="msg-b">
        <PendingSlip label={plain('summarizingRecording')} />
      </div>
    );
  return (
    <article className="msg" data-recording-id={recording.fileId}>
      <div className="msg-h" data-human data-has-chip>
        <span className="av" aria-hidden="true">
          {initialOf(authorName)}
        </span>
        <p className="txt">
          <b className="who">
            <LangRuns text={authorName} />
          </b>
        </p>
        <span className="msg-chip">
          <FileChip name={recording.name} size={recording.size} />
        </span>
        <time className="msg-time" dateTime={recording.at}>
          {formatClock(recording.at)}
        </time>
      </div>
      {slip}
    </article>
  );
}
