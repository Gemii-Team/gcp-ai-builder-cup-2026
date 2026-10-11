import { useEffect, useEffectEvent, useId, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from 'react';
import { Button } from '../Button';
import { NativeSelect, OwnerSelect, thaiLang, type Member } from '../draft/OwnerSelect';
import { isImeKey } from '../ime';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { DateField } from './DateField';

/** What a person entered: a confirmed task's title, channel, owner and due date (`YYYY-MM-DD`). */
export type TaskFormValues = { title: string; channelId: string; ownerId: string | null; dueDate: string | null };

export type TaskFormProps = {
  heading: string;
  /** The line under the heading: where the task comes from, or what a task added by hand is. */
  subtitle: ReactNode;
  /** The title the form opens with, selected, so typing replaces it. */
  initialTitle: string;
  /** The channels to choose from. Without it the form has no Channel select, and is the narrower message form. */
  channels?: readonly { id: string; name: string }[];
  channelId: string;
  /** The people of a channel, fetched when the form opens and again whenever its channel changes. */
  membersFor: (channelId: string) => Promise<readonly Member[]>;
  /** Creates the task. A rejection keeps the form open and shows an error. */
  onSubmit: (values: TaskFormValues) => Promise<void>;
  onCancel: () => void;
};

/**
 * A person makes a task directly, so it is confirmed at once: a plain slip with no perforation and no Draft tag, only a
 * title, a channel when there are several to choose from, an owner (who may be nobody) and a due date.
 */
export function TaskForm({ heading, subtitle, initialTitle, channels, channelId: initialChannelId, membersFor, onSubmit, onCancel }: TaskFormProps) {
  const labels = useLabels();
  const ids = useId();
  const [title, setTitle] = useState(initialTitle);
  const [channelId, setChannelId] = useState(initialChannelId);
  const [members, setMembers] = useState<readonly Member[]>([]);
  const [ownerId, setOwnerId] = useState<string | null>(null);
  const [dueDate, setDueDate] = useState<string | null>(null);
  const [invalid, setInvalid] = useState(false);
  const [failed, setFailed] = useState(false);
  const [sending, setSending] = useState(false);
  const titleField = useRef<HTMLInputElement>(null);

  useEffect(() => {
    titleField.current?.focus();
    titleField.current?.select();
  }, []);

  const loadMembers = useEffectEvent((id: string) => membersFor(id));
  useEffect(() => {
    let current = true;
    loadMembers(channelId).then(
      (list) => {
        if (current) {
          setMembers(list);
          // The owner of another channel is cleared; one who belongs to this channel too stays.
          setOwnerId((owner) => (list.some((member) => member.id === owner) ? owner : null));
        }
      },
      () => {
        if (current) {
          setMembers([]);
          setOwnerId(null);
        }
      },
    );
    return () => {
      current = false;
    };
  }, [channelId]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (sending) {
      return;
    }
    const trimmed = title.trim();
    if (trimmed === '') {
      setInvalid(true);
      titleField.current?.focus();
      return;
    }
    setSending(true);
    setFailed(false);
    try {
      await onSubmit({ title: trimmed, channelId, ownerId, dueDate });
    } catch {
      setFailed(true);
    } finally {
      setSending(false);
    }
  }

  function onKeyDown(event: KeyboardEvent<HTMLFormElement>) {
    if (isImeKey(event)) {
      // The Enter that commits a candidate must not become an implicit submit, and the Escape that dismisses one belongs
      // to the IME: cancelling the press here loses the title the person was composing.
      if (event.key === 'Enter') {
        event.preventDefault();
      }
      return;
    }
    if (event.key === 'Escape') {
      // The form may sit in a sheet or a list that closes on Escape: this one only closes the form.
      event.preventDefault();
      event.stopPropagation();
      onCancel();
    }
  }

  const headingId = `${ids}-heading`;
  const titleId = `${ids}-title`;
  const errorId = `${ids}-error`;
  return (
    <form
      className="kiwi-slip kiwi-mk"
      data-board={channels !== undefined || undefined}
      noValidate
      aria-labelledby={headingId}
      onSubmit={(event) => {
        void submit(event);
      }}
      onKeyDown={onKeyDown}
    >
      <div className="kiwi-mk-body">
        <div>
          <h3 id={headingId} className="kiwi-mk-t">
            <LangRuns text={heading} />
          </h3>
          <p className="kiwi-mk-s">{typeof subtitle === 'string' ? <LangRuns text={subtitle} /> : subtitle}</p>
        </div>
        <div className="kiwi-fld">
          <label htmlFor={titleId}>{labels.node('taskFieldTitle')}</label>
          <input
            ref={titleField}
            id={titleId}
            name="title"
            type="text"
            className="kiwi-input"
            maxLength={140}
            autoComplete="off"
            lang={thaiLang(title)}
            value={title}
            aria-invalid={invalid || undefined}
            aria-describedby={invalid ? errorId : undefined}
            onChange={(event) => {
              setTitle(event.target.value);
              setInvalid(false);
            }}
          />
          {invalid && (
            <p id={errorId} role="alert" className="kiwi-err">
              {labels.node('titleEmpty')}
            </p>
          )}
        </div>
        <div className="kiwi-mk-row">
          {channels !== undefined && (
            <div className="kiwi-fld">
              <label htmlFor={`${ids}-channel`}>{labels.node('taskFieldChannel')}</label>
              <NativeSelect
                id={`${ids}-channel`}
                value={channelId}
                options={channels.map((channel) => ({ value: channel.id, label: `# ${channel.name}` }))}
                onChange={setChannelId}
              />
            </div>
          )}
          <div className="kiwi-fld">
            <label htmlFor={`${ids}-owner`}>{labels.node('owner')}</label>
            <OwnerSelect id={`${ids}-owner`} members={members} value={ownerId} onChange={setOwnerId} warnWhenEmpty={false} />
          </div>
          <DateField label={labels.text('dueDate')} lang={labels.lang} value={dueDate} onChange={setDueDate} />
        </div>
        {failed && (
          <p role="alert" className="kiwi-err">
            {labels.node('createFailed')}
          </p>
        )}
      </div>
      <div className="kiwi-mk-foot">
        <Button variant="primary" size="sm" type="submit" aria-busy={sending || undefined}>
          {labels.node('createTask')}
        </Button>
        <Button size="sm" onClick={onCancel}>
          {labels.node('cancel')}
        </Button>
      </div>
    </form>
  );
}
