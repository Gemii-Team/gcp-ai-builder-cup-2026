import * as Dialog from '@radix-ui/react-dialog';
import { Icon, IconButton } from '@kiwi/ui';
import { useEffect, type ReactNode, type RefObject } from 'react';
import { T, useI18n } from '../../i18n';
import { useMediaQuery } from '../../lib/useMediaQuery';

type DraftsSheetProps = {
  /** The id of the sheet, which the drafts bar names in `aria-controls`. */
  id: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The element that opened the sheet: focus returns to it when the sheet closes. */
  opener: RefObject<HTMLElement | null>;
  /** The drafts panel. */
  children: ReactNode;
};

/**
 * The phone's drafts sheet: the drafts panel in a bottom sheet over a scrim, opened from the drafts bar. A Radix dialog:
 * the page behind it is inert, focus stays inside, the scrim and the close button close it and focus returns to the bar.
 * Escape closes an open excerpt first (the excerpt listens ahead of the dialog); it never closes the sheet from a text
 * field, where it belongs to the field (and to an IME that is composing). It closes itself when the screen becomes wide.
 */
export function DraftsSheet({ id, open, onOpenChange, opener, children }: DraftsSheetProps) {
  const { plain } = useI18n();
  const wide = useMediaQuery('(min-width: 1024px)');

  useEffect(() => {
    if (wide && open) {
      onOpenChange(false);
    }
  }, [wide, open, onOpenChange]);

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="scrim" />
        <Dialog.Content
          id={id}
          className="sheet"
          aria-describedby={undefined}
          onEscapeKeyDown={(event) => {
            if (event.isComposing || (event.target instanceof Element && event.target.closest('input, select, textarea') !== null)) {
              event.preventDefault();
            }
          }}
          onCloseAutoFocus={(event) => {
            event.preventDefault();
            opener.current?.focus();
          }}
        >
          <Dialog.Title className="sr-only">
            <T k="aiDrafts" />
          </Dialog.Title>
          <Dialog.Close asChild>
            <IconButton label={plain('close')} className="sheet-close">
              <Icon name="close" />
            </IconButton>
          </Dialog.Close>
          {children}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
