import { FailedSlip, PendingSlip } from '@kiwi/ui';
import { skipToken, useQuery, useQueryClient } from '@tanstack/react-query';
import { useApi } from '../../api/client';
import { useFile } from '../../api/hooks';
import { keys, meetingQuery } from '../../api/queries';
import type { ChatMessage, FileRefView, FileView, MeetingView } from '../../api/types';
import { useI18n } from '../../i18n';
import { failureReasonKey, isRecording } from '../../lib/failure';

type SlipProps = { file: FileRefView; message: ChatMessage };

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
      <div className="msg-b">
        <FailedSlip
          reason={plain('fileSummaryFailed', { name: file.name, reason: plain(failureReasonKey(status.failureReason)) })}
          failedAt={status.settledAt ?? message.createdAt}
          onRetry={() => {
            void api.files.retryAnalysis({ fileId: file.fileId }).then(() => {
              queryClient.setQueryData<FileView>(keys.file(file.fileId), (current) => current && { ...current, status: 'analyzing', failureReason: '' });
            });
          }}
        />
      </div>
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
      <div className="msg-b">
        <FailedSlip
          reason={plain('meetingSummaryFailed', { title: meeting.title, reason: plain(failureReasonKey(meeting.failureReason)) })}
          failedAt={meeting.settledAt ?? message.createdAt}
          onRetry={() => {
            void api.meetings.retrySummary({ meetingId: meeting.id }).then(() => {
              queryClient.setQueryData<MeetingView>(keys.meeting(meeting.id), (current) => current && { ...current, status: 'ingesting', failureReason: '' });
            });
          }}
        />
      </div>
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
