import { Icon, LangRuns } from '@kiwi/ui';
import { useId, useState } from 'react';
import type { ChannelView } from '../../api/types';
import { T, TCount, TSlots, useI18n } from '../../i18n';

type ScopeDisclosureProps = {
  /** The channels the question searches: all the person is in, or the one a `?channel=` scoped it to. */
  channels: readonly ChannelView[];
  /** The channel the question is scoped to, when it is: the line then names it instead of counting. */
  scopedTo: ChannelView | null;
  /** The id of the line's text, which the command field is described by. */
  textId: string;
};

/**
 * What is searched, always stated: `Searching 5 channels you're in`, opening to the channels as chips and the rule that only
 * channels the person belongs to are searched. At the right, in Carbon Copy, the languages Ask speaks (not on phones).
 */
export function ScopeDisclosure({ channels, scopedTo, textId }: ScopeDisclosureProps) {
  const { plain } = useI18n();
  const [open, setOpen] = useState(false);
  const listId = useId();
  return (
    <>
      <div className="ak-scope">
        <button
          type="button"
          className="ak-scope-b"
          aria-expanded={open}
          aria-controls={listId}
          onClick={() => {
            setOpen((now) => !now);
          }}
        >
          <span id={textId}>
            {scopedTo === null ? (
              <TCount k="askScope" count={channels.length} />
            ) : (
              <TSlots k="askScopedTo" slots={{ channel: <LangRuns text={scopedTo.name} /> }} />
            )}
          </span>
          <Icon name="chevron-down" className="ak-chev" />
        </button>
        <span className="ak-lang">
          <TSlots k="askLangHint" slots={{ thai: <span lang="th">ภาษาไทย</span> }} />
        </span>
      </div>
      <div className="ak-chans" id={listId} hidden={!open}>
        <ul className="ak-chans-l" aria-label={plain('askScopeList')}>
          {channels.map((channel) => (
            <li key={channel.id} className="ak-chip">
              # <LangRuns text={channel.name} />
            </li>
          ))}
        </ul>
        <p className="ak-note">
          <T k="askScopeNote" />
        </p>
      </div>
    </>
  );
}
