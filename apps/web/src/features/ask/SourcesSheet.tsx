import * as Dialog from '@radix-ui/react-dialog';
import { useEffect, type ReactNode, type RefObject } from 'react';
import { useMediaQuery } from '../../lib/useMediaQuery';

type SourcesSheetProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The marker, row link or Sources button that opened the sheet: focus returns to it when the sheet closes. */
  opener: RefObject<HTMLElement | null>;
  /** The Sources panel, whose heading is the dialog's title. */
  children: ReactNode;
};

/**
 * The phone's Sources sheet: the Sources panel in a bottom sheet over a scrim, opened by Sources or by pressing a citation.
 * A Radix dialog, like the drafts sheet: the page behind it is inert, focus stays inside, the scrim and the close button
 * close it and focus returns to the element that opened it, which the screen holds by reference. Escape closes an open
 * excerpt first (the excerpt listens ahead of the dialog); it never closes the sheet from a text field, where it belongs to
 * the field (and to an IME that is composing). It closes itself when the screen becomes wide.
 */
export function SourcesSheet({ open, onOpenChange, opener, children }: SourcesSheetProps) {
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
          {children}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
