import { formatSize } from '../format';
import { Icon } from '../icons';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';

/** An attached file: its icon, its name (wrapping anywhere) and a muted size. It sits in the author line of the message. */
export function FileChip({ name, size }: { name: string; size: number }) {
  return (
    <span className="inline-flex min-w-0 items-center gap-2 rounded-md border border-chip-edge bg-chip-fill px-2 text-meta leading-[22px]">
      <Icon name="file" />
      <span className="min-w-0 [overflow-wrap:anywhere]">
        <LangRuns text={name} />
      </span>
      <span className="-ml-1 whitespace-nowrap text-carbon-copy">
        · <Numeral>{formatSize(size)}</Numeral>
      </span>
    </span>
  );
}
