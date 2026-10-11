import { Button, Icon, NativeSelect, Numeral, Segmented } from '@kiwi/ui';
import { useId, useState, type Ref } from 'react';
import type { ChannelView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { STATUS_KEY, STATUSES, type BoardFilters, type Status } from './useBoardData';

const SOURCE_TYPES = [
  { value: 'meeting', label: 'sourceMeetings' },
  { value: 'message', label: 'sourceMessages' },
  { value: 'file', label: 'sourceDocuments' },
  { value: 'hand', label: 'sourceByHand' },
] as const;

type FilterBarProps = {
  channels: readonly ChannelView[];
  filters: BoardFilters;
  onChange: (filters: BoardFilters) => void;
  /** On phones the column heads give way to a three-way status control above a Filters button. */
  phone: boolean;
  status: Status;
  counts: Record<Status, number>;
  onStatus: (status: Status) => void;
  /** The bar's root, so the Board can hand focus to the status control after a card leaves the status on show. */
  ref?: Ref<HTMLDivElement>;
};

/**
 * The Channel and Source type selects (the owner select reused as a filter). A filter narrows the lanes, never the AI
 * drafts, which are always all there. On phones the selects wait behind one Filters button, in two columns, under the
 * Status control (To do, Doing, Done with their counts).
 */
export function FilterBar({ channels, filters, onChange, phone, status, counts, onStatus, ref }: FilterBarProps) {
  const { plain } = useI18n();
  const panelId = useId();
  const [open, setOpen] = useState(false);
  const active = [filters.channelId, filters.sourceType].filter((value) => value !== null).length;
  return (
    <div ref={ref} className="bd-tool" role="group" aria-label={plain('boardFilters')}>
      {phone && (
        <>
          <Segmented
            variant="status"
            label={plain('boardStatus')}
            value={status}
            onChange={onStatus}
            options={STATUSES.map((value) => ({ value, label: <T k={STATUS_KEY[value]} />, count: counts[value] }))}
          />
          <div className="bd-filt-row">
            <Button
              aria-expanded={open}
              aria-controls={panelId}
              onClick={() => {
                setOpen((was) => !was);
              }}
            >
              <Icon name="filter" />
              <T k="filtersToggle" />
              {active > 0 && <Numeral>({active})</Numeral>}
            </Button>
          </div>
        </>
      )}
      <div className="bd-tool-f" id={panelId} hidden={phone && !open}>
        <NativeSelect
          aria-label={plain('filterChannel')}
          value={filters.channelId ?? ''}
          emptyLabel={plain('allChannels')}
          options={channels.map((channel) => ({ value: channel.id, label: `# ${channel.name}` }))}
          onChange={(channelId) => {
            onChange({ ...filters, channelId: channelId === '' ? null : channelId });
          }}
        />
        <NativeSelect
          aria-label={plain('filterSource')}
          value={filters.sourceType ?? ''}
          emptyLabel={plain('allSources')}
          options={SOURCE_TYPES.map(({ value, label }) => ({ value, label: plain(label) }))}
          onChange={(sourceType) => {
            onChange({ ...filters, sourceType: SOURCE_TYPES.find((type) => type.value === sourceType)?.value ?? null });
          }}
        />
      </div>
    </div>
  );
}
