import { FailedSlip, Icon, PendingSlip } from '@kiwi/ui';
import { skipToken, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi } from '../../api/client';
import { useFile } from '../../api/hooks';
import { keys, meetingQuery } from '../../api/queries';
import type { ChatMessage, FileRefView, FileView, MeetingView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { failureReasonKey, isRecording } from '../../lib/failure';

type SlipProps = { file: FileRefView; message: ChatMessage };

type FailureProps = {
  reason: string;
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
        reason={plain('fileSummaryFailed', { name: file.name, reason: plain(failureReasonKey(status.failureReason)) })}
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

/** A recording this session uploaded, under its message: the meeting it became is being summarized, or could not be. */
function RecordingSlip({ file, message }: SlipProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { plain } = useI18n();
  const meetingId = useQuery({ queryKey: keys.recording(file.fileId), queryFn: skipToken, staleTime: Infinity }).data as string | undefined;
  const meeting = useQuery({ ...meetingQuery(api, meetingId ?? ''), enabled: meetingId !== undefined }).data;

  if (meeting === undefined || meeting.status === 'ready') {
    return null;
  }
  if (meeting.status === 'failed' || meeting.status === 'no_transcript') {
    return (
      <Failure
        reason={plain('meetingSummaryFailed', { title: meeting.title, reason: plain(failureReasonKey(meeting.failureReason)) })}
        failedAt={meeting.settledAt ?? message.createdAt}
        retry={async () => {
          await api.meetings.retrySummary({ meetingId: meeting.id });
          queryClient.setQueryData<MeetingView>(keys.meeting(meeting.id), (current) => current && { ...current, status: 'ingesting', failureReason: '' });
        }}
      />
    );
  }
  return (
    <div className="msg-b">
      <PendingSlip label={plain('summarizingRecording')} />
    </div>
  );
}

/** The status slips of the files a message carries, derived from the file and meeting caches that realtime events keep current. */
export function MessageFiles({ message }: { message: ChatMessage }) {
  return message.files.map((file) =>
    isRecording(file.mime) ? <RecordingSlip key={file.fileId} file={file} message={message} /> : <FileSlip key={file.fileId} file={file} message={message} />,
  );
}
