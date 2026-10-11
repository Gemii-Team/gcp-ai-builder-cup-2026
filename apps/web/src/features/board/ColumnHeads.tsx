import { Numeral } from '@kiwi/ui';
import { T } from '../../i18n';
import { STATUS_KEY, STATUSES, type Status } from './useBoardData';

/** The three heads that stay pinned while the lanes scroll: a swatch, the status and its count in mono. */
export function ColumnHeads({ counts }: { counts: Record<Status, number> }) {
  return (
    <div className="bd-cols">
      {STATUSES.map((status) => (
        <div key={status} className="bd-col-h">
          <i className={`bd-sw bd-sw-${status}`} aria-hidden="true" />
          <T k={STATUS_KEY[status]} />
          <Numeral className="bd-n">{counts[status]}</Numeral>
        </div>
      ))}
    </div>
  );
}
