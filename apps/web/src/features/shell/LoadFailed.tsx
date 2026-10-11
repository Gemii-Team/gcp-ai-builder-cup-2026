import { Button, Icon } from '@kiwi/ui';
import type { ReactNode } from 'react';
import { T } from '../../i18n';

/** A load that failed, said once, with Retry: the one failure state of the shell, the start page and the rail. */
export function LoadFailed({ children, onRetry }: { children: ReactNode; onRetry: () => void }) {
  return (
    <div className="load-failed" role="alert">
      <Icon name="warning" />
      <p>{children}</p>
      <Button size="sm" onClick={onRetry}>
        <T k="retry" />
      </Button>
    </div>
  );
}
