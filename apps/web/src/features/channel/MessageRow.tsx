import { Button, FileChip, formatClock, Icon, LangRuns, MarkSmall } from '@kiwi/ui';
import type { ReactNode } from 'react';
import type { ArtifactView, ChatMessage, MentionView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { initialOf } from '../../lib/text';
import { ArtifactSlip } from './ArtifactSlip';

const escapeRegExp = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/** The text of a message: Thai runs tagged `lang="th"`, and each `@mention` the server recognised set in Citation Teal. */
function Body({ body, mentions }: { body: string; mentions: readonly MentionView[] }) {
  if (mentions.length === 0) {
    return <LangRuns text={body} />;
  }
  const names = [...mentions].sort((a, b) => b.label.length - a.label.length).map((mention) => escapeRegExp(mention.label));
  return body.split(new RegExp(`(@(?:${names.join('|')}))`, 'u')).map((part, index) =>
    index % 2 === 1 ? (
      <span key={index} className="at">
        <LangRuns text={part} />
      </span>
    ) : (
      <LangRuns key={index} text={part} />
    ),
  );
}

/** What a message that has not reached the server yet says about itself, with Retry once the server has refused it. */
function Delivery({ delivery, onRetry }: { delivery: NonNullable<ChatMessage['delivery']>; onRetry?: () => void }) {
  if (delivery === 'failed') {
    return (
      <p className="delivery" data-failed="" role="alert">
        <Icon name="warning" />
        <T k="deliveryFailed" />
        {onRetry !== undefined && (
          <Button size="sm" onClick={onRetry}>
            <T k="retrySend" />
          </Button>
        )}
      </p>
    );
  }
  return (
    <p className="delivery" role="status">
      <Icon name="clock" />
      <T k={delivery === 'sending' ? 'deliverySending' : 'deliveryQueued'} />
    </p>
  );
}

/** The kind label of an AI summary; an answer is the assistant's own message and has none. */
function kindLabel(kind: ArtifactView['kind']) {
  switch (kind) {
    case 'meeting_summary':
      return 'kindMeetingSummary';
    case 'doc_summary':
      return 'kindFileSummary';
    case 'channel_summary':
      return 'kindChannelSummary';
    default:
      return undefined;
  }
}

type MessageRowProps = {
  message: ChatMessage;
  /** The author's name; ignored for an agent, which has one constant label. */
  authorName: string;
  /** For a meeting summary: how many drafts it made that still wait on a person. */
  draftCount?: number;
  /** At the row's top right, left of the time: Make a task, on a human message. */
  action?: ReactNode;
  /** Under the message, aligned with its body: the status slips of its files, a task made from it. */
  below?: ReactNode;
  onRetry?: () => void;
};

/**
 * One message: a 24px avatar, the author (a person by name, the agent as `@assistant`, an AI summary as K.I.W.I. with
 * its kind), the text, the files it carries and the time of day, then, indented under the name, its slip or status.
 * A message with an artifact is a proof slip under the AI mark; it is never a person's, so it never offers Make a task.
 */
export function MessageRow({ message, authorName, draftCount, action, below, onRetry }: MessageRowProps) {
  const { plain } = useI18n();
  const { artifact } = message;
  const human = message.authorKind === 'user';
  const kind = artifact === undefined ? undefined : kindLabel(artifact.kind);

  return (
    <article className="msg" data-message-id={message.id}>
      <div className="msg-h" data-human={human || undefined} data-action={action !== undefined || undefined} data-has-chip={message.files.length > 0 || undefined}>
        {human ? (
          <span className="av" aria-hidden="true">
            {initialOf(authorName)}
          </span>
        ) : (
          <MarkSmall size={24} />
        )}
        <p className="txt">
          <b className="who">
            {human ? <LangRuns text={authorName} /> : <span lang="en">{kind === undefined ? '@assistant' : 'K.I.W.I.'}</span>}
          </b>
          {kind !== undefined && <span className="kind">{plain(kind)}</span>}
          {message.body !== '' && artifact === undefined && <Body body={message.body} mentions={message.mentions} />}
        </p>
        {message.files.map((file) => (
          <span key={file.fileId} className="msg-chip">
            <FileChip name={file.name} size={file.size} />
          </span>
        ))}
        {action}
        <time className="msg-time" dateTime={message.createdAt}>
          {formatClock(message.createdAt)}
        </time>
      </div>
      {artifact !== undefined && (
        <div className="msg-b">
          <ArtifactSlip artifact={artifact} draftCount={draftCount} />
        </div>
      )}
      {message.delivery !== undefined && (
        <div className="msg-b">
          <Delivery delivery={message.delivery} onRetry={onRetry} />
        </div>
      )}
      {below}
    </article>
  );
}
