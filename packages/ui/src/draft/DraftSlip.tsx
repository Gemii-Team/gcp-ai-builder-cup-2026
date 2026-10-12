import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { Button, ButtonLink } from '../Button';
import { formatOffset } from '../format';
import { isImeKey } from '../ime';
import { Icon } from '../icons';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { ExcerptRow, type Source } from '../slip/ExcerptContext';
import { Perforation } from '../slip/Perforation';
import { RefCode } from '../slip/RefCode';
import { Slip, SlipBody } from '../slip/Slip';
import { OffsetLink, PageLink } from '../slip/SourceLinks';
import { Stub } from '../slip/Stub';
import { OwnerSelect, thaiLang, type Member } from './OwnerSelect';

/** A task the AI drafted and a person has not yet confirmed. Mirrors `tasks.v1.Task` with plain strings and numbers. */
export type DraftTask = {
  id: string;
  /** The REF code without its label: `TSK-2031`. */
  ref: string;
  title: string;
  ownerId: string | null;
  dueLabel?: string;
  /** What the AI drafted it from: the meeting moment, the document page or the message. */
  source: Source;
  /** The answer the draft was made from, `ANS-2209`, when it came from Ask. */
  fromAnswerRef?: string;
  /** Who this draft may be given to, when that differs from the panel's list: the Board's drafts belong to different channels. */
  members?: readonly Member[];
};

/** What a person may change on a draft before confirming it. */
export type DraftPatch = { title?: string; ownerId?: string | null };

type DraftSlipProps = {
  draft: DraftTask;
  members: readonly Member[];
  /** Saves a change; a rejection keeps the slip as it was and shows the error. */
  onEdit: (patch: DraftPatch) => Promise<void>;
} & (
  | { mode?: 'review'; onConfirm: () => Promise<void> }
  | { mode: 'handoff'; boardHref: string; onDiscard: () => void }
);

/** `From answer ANS-2209`: the answer's REF in the REF voice, without the `REF` label a task's own code carries. */
function AnswerRef({ code }: { code: string }) {
  return (
    <span lang="en" className="font-mono text-reference text-primary whitespace-nowrap">
      {code}
    </span>
  );
}

/** Where the draft came from: `From meeting · 0:21:47`, `ACME-contract-v3.pdf · p. 4` or `From answer ANS-2209 · 0:33:18`. */
function From({ draft, withLink }: { draft: DraftTask; withLink: boolean }) {
  const labels = useLabels();
  const { source, fromAnswerRef } = draft;
  let lead: ReactNode;
  if (fromAnswerRef !== undefined) {
    lead = labels.node('fromAnswer', { ref: <AnswerRef code={fromAnswerRef} /> });
  } else if (source.kind === 'meeting') {
    lead = labels.node('fromMeeting');
  } else if (source.kind === 'file') {
    lead = <LangRuns text={source.label} />;
  } else {
    lead = labels.node('fromMessage');
  }
  // A missing offset or page renders no link, so it must not leave a dangling separator either.
  let where: ReactNode = null;
  if (withLink && source.kind === 'meeting' && formatOffset(source.offsetMs) !== null) {
    where = <OffsetLink source={source} />;
  } else if (withLink && source.kind === 'file' && source.page !== undefined) {
    where = <PageLink source={source} />;
  }
  return (
    <span className="kiwi-from">
      {lead}
      {where !== null && <> · {where}</>}
    </span>
  );
}

/**
 * An AI draft: a proof slip with the Draft tag, so it keeps its perforation. Confirm and Edit sit in the stub; Confirm
 * is disabled until there is an owner. `mode="handoff"` is Ask's variant, which can only be reviewed on the Board.
 */
export function DraftSlip(props: DraftSlipProps) {
  const { draft, members, onEdit } = props;
  const labels = useLabels();
  const errorId = useId();
  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState('');
  const [invalid, setInvalid] = useState(false);
  const [failure, setFailure] = useState<'confirm' | 'save' | null>(null);
  const [busy, setBusy] = useState(false);
  const field = useRef<HTMLInputElement>(null);
  const editButton = useRef<HTMLButtonElement>(null);
  const restoreFocus = useRef(false);

  useEffect(() => {
    if (editing) {
      field.current?.focus();
      field.current?.select();
    } else if (restoreFocus.current) {
      restoreFocus.current = false;
      editButton.current?.focus();
    }
  }, [editing]);

  async function save(change: DraftPatch): Promise<boolean> {
    setFailure(null);
    try {
      await onEdit(change);
      return true;
    } catch {
      setFailure('save');
      return false;
    }
  }

  function startEdit() {
    setTitle(draft.title);
    setInvalid(false);
    setFailure(null);
    setEditing(true);
  }

  async function finishEdit(keep: boolean) {
    if (keep) {
      const next = title.trim();
      if (next === '') {
        setInvalid(true);
        field.current?.focus();
        return;
      }
      if (next !== draft.title && !(await save({ title: next }))) {
        return;
      }
    }
    restoreFocus.current = true;
    setInvalid(false);
    setEditing(false);
  }

  function onFieldKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (isImeKey(event)) {
      // The Enter that commits an IME candidate and the Escape that dismisses one belong to the IME.
      return;
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      void finishEdit(true);
    } else if (event.key === 'Escape') {
      // The slip may sit in a sheet that closes on Escape: this one only cancels the edit.
      event.preventDefault();
      event.stopPropagation();
      void finishEdit(false);
    }
  }

  async function confirm() {
    if (busy || props.mode === 'handoff') {
      return;
    }
    setBusy(true);
    setFailure(null);
    try {
      await props.onConfirm();
    } catch {
      setFailure('confirm');
      setBusy(false);
    }
  }

  const hasOwner = draft.ownerId !== null;
  const handoff = props.mode === 'handoff';
  return (
    <Slip className="kiwi-draft" data-has-owner={hasOwner || undefined}>
      <span className="kiwi-tag">{labels.node('draft')}</span>
      <SlipBody>
        {editing ? (
          <>
            <input
              ref={field}
              type="text"
              className="kiwi-edit"
              aria-label={labels.text('taskTitle')}
              aria-invalid={invalid || undefined}
              aria-describedby={invalid ? errorId : undefined}
              lang={thaiLang(title)}
              value={title}
              onChange={(event) => {
                setTitle(event.target.value);
                setInvalid(false);
              }}
              onKeyDown={onFieldKeyDown}
            />
            {invalid && (
              <p id={errorId} role="alert" className="kiwi-err">
                {labels.node('titleEmpty')}
              </p>
            )}
          </>
        ) : (
          <h3 className="kiwi-d-title">
            <LangRuns text={draft.title} />
          </h3>
        )}
        <p className="kiwi-d-meta">
          <OwnerSelect
            aria-label={labels.text('owner')}
            members={members}
            value={draft.ownerId}
            onChange={(ownerId) => {
              void save({ ownerId });
            }}
          />{' '}
          <span className="kiwi-due">{draft.dueLabel === undefined ? labels.node('noDueDate') : <LangRuns text={draft.dueLabel} />}</span>
        </p>
      </SlipBody>
      <Perforation />
      <Stub className="kiwi-draft-stub">
        <ExcerptRow className="kiwi-row">
          <RefCode code={draft.ref} />
          <From draft={draft} withLink={!handoff} />
        </ExcerptRow>
        {props.mode === 'handoff' && <p className="kiwi-note">{labels.node('handoffNote')}</p>}
        <div className="kiwi-acts">
          {props.mode === 'handoff' && (
            <>
              <ButtonLink variant="primary" size="sm" href={props.boardHref}>
                {labels.node('reviewOnBoard')}
                <Icon name="arrow-right" size={16} />
              </ButtonLink>
              <Button size="sm" onClick={props.onDiscard}>
                {labels.node('discard')}
              </Button>
            </>
          )}
          {props.mode !== 'handoff' && editing && (
            <>
              <Button
                variant="primary"
                size="sm"
                onClick={() => {
                  void finishEdit(true);
                }}
              >
                {labels.node('save')}
              </Button>
              <Button
                size="sm"
                onClick={() => {
                  void finishEdit(false);
                }}
              >
                {labels.node('cancel')}
              </Button>
            </>
          )}
          {props.mode !== 'handoff' && !editing && (
            <>
              <Button
                variant="primary"
                size="sm"
                data-confirm=""
                aria-label={labels.text('confirmNamed', { title: draft.title })}
                aria-busy={busy || undefined}
                disabled={!hasOwner}
                disabledReason={labels.text('chooseOwnerFirst')}
                onClick={() => {
                  void confirm();
                }}
              >
                {labels.node('confirmAction')}
              </Button>
              <Button ref={editButton} size="sm" aria-label={labels.text('editNamed', { title: draft.title })} onClick={startEdit}>
                {labels.node('edit')}
              </Button>
            </>
          )}
        </div>
        {failure !== null && (
          <p role="alert" className="kiwi-err">
            {labels.node(failure === 'confirm' ? 'confirmFailed' : 'saveFailed')}
          </p>
        )}
      </Stub>
    </Slip>
  );
}
