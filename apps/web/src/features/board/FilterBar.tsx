import { NativeSelect } from '@kiwi/ui';
import type { ChannelView } from '../../api/types';
import { useI18n } from '../../i18n';
import type { BoardFilters } from './useBoardData';

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
};

/**
 * The Channel and Source type selects (the owner select reused as a filter). A filter narrows the lanes, never the AI
 * drafts, which are always all there.
 */
export function FilterBar({ channels, filters, onChange }: FilterBarProps) {
  const { plain } = useI18n();
  return (
    <div className="bd-tool" role="group" aria-label={plain('boardFilters')}>
      <div className="bd-tool-f">
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
