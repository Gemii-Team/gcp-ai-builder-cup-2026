import { Button, Icon } from '@kiwi/ui';
import { useId, useLayoutEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import type { MentionView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { MentionList } from './MentionList';
import { mentionOptionId, type MentionTarget } from './mentions';

export type { MentionTarget } from './mentions';

/** `@` and what follows it, up to the caret: the mention being typed. */
const OPEN_MENTION = /(^|\s)@([^\s@]*)$/;

const escapeRegExp = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/** The people the message names with `@`, in the order it names them, as the server takes them: the agent by its constant label, a member by id. */
function mentionsIn(body: string, targets: readonly MentionTarget[]): MentionView[] {
  return targets
    .map((target) => ({ target, at: new RegExp(`(^|\\s)@${escapeRegExp(target.name)}(?=$|[\\s,!?:;])`, 'u').exec(body)?.index }))
    .flatMap(({ target, at }) => (at === undefined ? [] : [{ at, mention: { kind: target.kind, id: target.id, label: target.name } }]))
    .sort((a, b) => a.at - b.at)
    .map(({ mention }) => mention);
}

type ComposerProps = {
  channelName: string;
  /** Who `@` offers: the agent first, then the channel's members. */
  people: readonly MentionTarget[];
  /** Whether a message can be sent now (the stream it joins has loaded). */
  canSend: boolean;
  onSend: (body: string, mentions: MentionView[]) => Promise<void>;
  /** The paperclip and what it needs: sits between the field and Send. */
  attach?: ReactNode;
  /** What went wrong with the last attachment, shown above the field. */
  notice?: ReactNode;
};

/**
 * The message field with its `@` list, an attach slot and Send. Enter sends, but never while a Thai IME is composing: the
 * Enter that commits a candidate (and the Escape that cancels one) belongs to the IME, so a half-typed message is neither
 * sent nor cut short. The list opens on `@`; the arrow keys move in it, Enter picks and Escape closes it.
 */
export function Composer({ channelName, people, canSend, onSend, attach, notice }: ComposerProps) {
  const { plain } = useI18n();
  const phone = useMediaQuery('(max-width: 600px)');
  const [value, setValue] = useState('');
  const [caret, setCaret] = useState(0);
  const [active, setActive] = useState(0);
  const [dismissed, setDismissed] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const nextCaret = useRef<number | null>(null);
  const listId = useId();

  const query = OPEN_MENTION.exec(value.slice(0, caret))?.[2];
  const shown = query === undefined || dismissed ? [] : people.filter((person) => person.name.toLowerCase().includes(query.toLowerCase()));
  const open = shown.length > 0;
  const current = Math.min(active, shown.length - 1);

  useLayoutEffect(() => {
    if (nextCaret.current !== null) {
      input.current?.setSelectionRange(nextCaret.current, nextCaret.current);
      nextCaret.current = null;
    }
  });

  function pick(index: number) {
    const target = shown[index];
    if (target === undefined) {
      return;
    }
    const head = value.slice(0, caret).replace(/@([^\s@]*)$/, `@${target.name} `);
    setValue(head + value.slice(caret));
    setCaret(head.length);
    nextCaret.current = head.length;
    setActive(0);
    input.current?.focus();
  }

  function submit() {
    const body = value.trim();
    if (body === '' || !canSend) {
      return;
    }
    setValue('');
    setCaret(0);
    setDismissed(false);
    void onSend(body, mentionsIn(body, people));
  }

  function onKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    // A key that belongs to the IME: `isComposing` while it composes, keyCode 229 on the Enter that Safari delivers just after.
    if (event.nativeEvent.isComposing || event.nativeEvent.keyCode === 229) {
      return;
    }
    if (open) {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault();
        setActive((current + (event.key === 'ArrowDown' ? 1 : shown.length - 1)) % shown.length);
      } else if (event.key === 'Enter') {
        event.preventDefault();
        pick(current);
      } else if (event.key === 'Escape') {
        event.preventDefault();
        setDismissed(true);
      }
    } else if (event.key === 'Enter') {
      event.preventDefault();
      submit();
    }
  }

  return (
    <div className="composer">
      {open && <MentionList id={listId} targets={shown} active={current} onPick={pick} />}
      {notice}
      <form
        className="cp"
        autoComplete="off"
        onSubmit={(event) => {
          event.preventDefault();
          submit();
        }}
      >
        <input
          ref={input}
          type="text"
          role="combobox"
          aria-label={plain('composerLabel', { channel: channelName })}
          aria-autocomplete="list"
          aria-expanded={open}
          aria-controls={open ? listId : undefined}
          aria-activedescendant={open ? mentionOptionId(listId, current) : undefined}
          enterKeyHint="send"
          spellCheck={false}
          placeholder={plain(phone ? 'composerPlaceholderPhone' : 'composerPlaceholder', { channel: channelName })}
          value={value}
          onChange={(event) => {
            setValue(event.target.value);
            setCaret(event.target.selectionStart ?? event.target.value.length);
            setActive(0);
            setDismissed(false);
          }}
          onSelect={(event) => {
            setCaret(event.currentTarget.selectionStart ?? value.length);
          }}
          onKeyDown={onKeyDown}
        />
        {attach}
        <Button variant="primary" type="submit" disabled={value.trim() === '' || !canSend}>
          <Icon name="send" />
          <span className="send-label">
            <T k="sendMessage" />
          </span>
        </Button>
      </form>
    </div>
  );
}
