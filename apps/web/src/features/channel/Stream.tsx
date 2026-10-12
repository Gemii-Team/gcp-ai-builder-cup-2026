import { Button, Pending, usePrefersReducedMotion } from '@kiwi/ui';
import { Fragment, useLayoutEffect, useRef, type ReactNode } from 'react';
import type { ChannelView, ChatMessage } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { dayKey, dayLabel } from '../../lib/day';

/** How near the foot the reader must be for a new message to keep them at the foot. */
const NEAR_BOTTOM_PX = 96;

type StreamProps = {
  channel: ChannelView;
  /** Oldest first; undefined until the first page has loaded. */
  messages: readonly ChatMessage[] | undefined;
  failed: boolean;
  onReload: () => void;
  hasEarlier: boolean;
  loadingEarlier: boolean;
  earlierFailed: boolean;
  onLoadEarlier: () => void;
  /** One message as a row: the screen wires the row's author, actions and slips. */
  renderRow: (message: ChatMessage) => ReactNode;
  /** What follows the last message and is not one: the status rows of this session's recordings. */
  tail?: ReactNode;
  /** How many rows `tail` holds, so a reader at the foot is kept there when one is added. */
  tailCount?: number;
};

/**
 * The messages of a channel, bottom-anchored: the newest sits at the foot, the reader stays where they are when older
 * messages are added above, and a new message keeps them at the foot only if they were already there (their own always does).
 * Days are divided by a label and a hairline; `Load earlier` adds the page before the oldest message held.
 */
export function Stream({ channel, messages, failed, onReload, hasEarlier, loadingEarlier, earlierFailed, onLoadEarlier, renderRow, tail, tailCount = 0 }: StreamProps) {
  const { plain, lang } = useI18n();
  const reduced = usePrefersReducedMotion();
  const scroller = useRef<HTMLDivElement>(null);
  const atFoot = useRef(true);
  const seen = useRef<{ first?: string; last?: string; height: number }>({ height: 0 });
  const first = messages?.[0]?.id;
  const last = messages?.at(-1);

  useLayoutEffect(() => {
    const element = scroller.current;
    if (element === null) {
      return;
    }
    const before = seen.current;
    const olderAdded = before.first !== undefined && first !== before.first && last?.id === before.last;
    if (olderAdded) {
      // Keep what the reader was looking at in place while the older page pushes it down.
      element.scrollTop += element.scrollHeight - before.height;
    } else if (atFoot.current || last?.delivery !== undefined) {
      element.scrollTo?.({ top: element.scrollHeight, behavior: before.last === undefined || last?.delivery !== undefined || reduced ? 'auto' : 'smooth' });
      atFoot.current = true;
    }
    seen.current = { first, last: last?.id, height: element.scrollHeight };
  }, [first, last?.id, last?.delivery, tailCount, reduced]);

  const now = new Date();
  let previousDay = '';
  return (
    <div
      ref={scroller}
      className="stream"
      role="log"
      aria-label={plain('messagesIn', { channel: channel.name })}
      tabIndex={0}
      onScroll={(event) => {
        const { scrollHeight, scrollTop, clientHeight } = event.currentTarget;
        atFoot.current = scrollHeight - scrollTop - clientHeight < NEAR_BOTTOM_PX;
      }}
    >
      <div className="stream-in">
        {messages === undefined && !failed && <Pending label={plain('loadingMessages')} />}
        {failed && (
          <p className="notice" role="alert">
            <T k="loadMessagesFailed" />
            <Button size="sm" onClick={onReload}>
              <T k="retry" />
            </Button>
          </p>
        )}
        {hasEarlier && (
          <div className="earlier">
            <Button size="sm" aria-busy={loadingEarlier || undefined} disabled={loadingEarlier} onClick={onLoadEarlier}>
              <T k="loadEarlier" />
            </Button>
            {earlierFailed && (
              <p className="notice" role="alert">
                <T k="loadEarlierFailed" />
              </p>
            )}
          </div>
        )}
        {messages?.map((message) => {
          const day = dayKey(message.createdAt);
          const divider = day === previousDay ? null : dayLabel(message.createdAt, lang, now);
          previousDay = day;
          return (
            <Fragment key={message.id}>
              {divider !== null && (
                <div className="day">
                  <span>{divider.today ? <T k="today" params={{ day: divider.text }} /> : divider.text}</span>
                </div>
              )}
              {renderRow(message)}
            </Fragment>
          );
        })}
        {tail}
      </div>
    </div>
  );
}
