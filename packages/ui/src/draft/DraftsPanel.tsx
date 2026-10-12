import { useId, useRef } from 'react';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import { DraftSlip, type DraftPatch, type DraftTask } from './DraftSlip';
import { DraftsEmpty } from './DraftsEmpty';
import type { Member } from './OwnerSelect';
import { SealedSlip, type SealedTask } from './SealedSlip';
import { useTearAndSeal, type TearAndSeal } from './useTearAndSeal';

/** The Board's task counts per status. */
export type BoardCounts = { todo: number; doing: number; done: number };

type DraftsPanelProps = {
  drafts: readonly DraftTask[];
  /** Drafts confirmed a moment ago, newest first. */
  sealed: readonly SealedTask[];
  /** The people a draft may be given to. */
  members: readonly Member[];
  /** Confirms a draft on the server. Resolve once it has agreed; the app then moves the draft from `drafts` to `sealed`. */
  onConfirm: (id: string) => Promise<void>;
  onEdit: (id: string, patch: DraftPatch) => Promise<void>;
  counts: BoardCounts;
  /**
   * `panel` is the channel's right column, with the Board's counts at its foot. `band` is the Board's drafts band: the
   * heading with its caption and count, the slips in the Board's columns, no counts, and the empty state as a row.
   */
  variant: 'panel' | 'band';
};

function BoardBlock({ counts }: { counts: BoardCounts }) {
  const labels = useLabels();
  const headingId = useId();
  const rows = [
    { status: 'todo', label: labels.node('todo'), count: counts.todo },
    { status: 'doing', label: labels.node('doing'), count: counts.doing },
    { status: 'done', label: labels.node('done'), count: counts.done },
  ] as const;
  return (
    <section className="kiwi-board" aria-labelledby={headingId}>
      <h3 id={headingId}>{labels.node('board')}</h3>
      <ul>
        {rows.map((row) => (
          <li key={row.status}>
            <i data-status={row.status} aria-hidden="true" />
            <span>{row.label}</span>
            <Numeral>{row.count}</Numeral>
          </li>
        ))}
      </ul>
      {/* Each segment is as wide as its count: a status with none takes no width, and no count divides by a total. */}
      <div className="kiwi-bar" aria-hidden="true">
        {rows.map((row) => (
          <i key={row.status} data-status={row.status} style={{ flex: row.count }} />
        ))}
      </div>
    </section>
  );
}

type DraftItemProps = Pick<DraftsPanelProps, 'members' | 'onConfirm' | 'onEdit'> & {
  draft: DraftTask;
  confirm: TearAndSeal['confirm'];
};

function DraftItem({ draft, members, onConfirm, onEdit, confirm }: DraftItemProps) {
  const item = useRef<HTMLLIElement>(null);
  return (
    <li ref={item} className="kiwi-item" data-draft="" data-flip="" data-task-id={draft.id}>
      <DraftSlip
        draft={draft}
        members={draft.members ?? members}
        onEdit={(patch) => onEdit(draft.id, patch)}
        onConfirm={() => (item.current === null ? Promise.resolve() : confirm(item.current, () => onConfirm(draft.id)))}
      />
    </li>
  );
}

/**
 * The drafts that wait for a person, and the ones just confirmed. Confirming goes through `useTearAndSeal`: the slip
 * stays a draft until the server has agreed, and only then tears, seals and glides.
 */
export function DraftsPanel({ drafts, sealed, members, onConfirm, onEdit, counts, variant }: DraftsPanelProps) {
  const labels = useLabels();
  const headingId = useId();
  const { confirm, announcement } = useTearAndSeal();
  return (
    <aside className="kiwi-panel" data-drafts-root="" data-variant={variant} aria-labelledby={headingId}>
      <div className="kiwi-ph">
        <h2 id={headingId}>{labels.node('aiDrafts')}</h2>
        {variant === 'band' && <span className="kiwi-cap">{labels.node('draftsCaption')}</span>}
        <span className="kiwi-pc" data-zero={drafts.length === 0 || undefined}>
          {drafts.length === 0 ? labels.node('allConfirmed') : labels.node('toConfirm', { count: drafts.length })}
        </span>
      </div>
      <div className="kiwi-pbody" data-drafts-body="">
        {sealed.length > 0 && (
          <ul className="kiwi-slips" data-sealed-list="" aria-label={labels.text(variant === 'band' ? 'confirmedJustNow' : 'confirmed')}>
            {sealed.map(({ id, ...task }) => (
              <li key={id} className="kiwi-item" data-flip="" data-task-id={id}>
                <SealedSlip {...task} />
              </li>
            ))}
          </ul>
        )}
        {drafts.length > 0 ? (
          <ul className="kiwi-slips" data-drafts-list="" aria-label={labels.text('draftsList')}>
            {drafts.map((draft) => (
              <DraftItem key={draft.id} draft={draft} members={members} onConfirm={onConfirm} onEdit={onEdit} confirm={confirm} />
            ))}
          </ul>
        ) : (
          <DraftsEmpty variant={variant} />
        )}
        {variant === 'panel' && <BoardBlock counts={counts} />}
      </div>
      <p role="status" className="sr-only">
        <LangRuns text={announcement} />
      </p>
    </aside>
  );
}
