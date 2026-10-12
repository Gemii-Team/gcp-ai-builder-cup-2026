import * as Dialog from '@radix-ui/react-dialog';
import { Button, Icon, IconButton, LangRuns, Numeral } from '@kiwi/ui';
import type { RefObject } from 'react';
import type { SessionView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { initialOf } from '../../lib/text';
import { useSignOut } from './useSignOut';

type AccountSheetProps = {
  /** The id of the sheet, which the Me tab names in `aria-controls`. */
  id: string;
  session: SessionView;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The Me tab: focus returns to it when the sheet closes. */
  opener: RefObject<HTMLElement | null>;
};

/**
 * The phone's account sheet, opened from the Me tab: an Account heading with its 44px close button, the person's card
 * and a full-width Sign out. A Radix dialog, so the page behind it is inert, focus stays inside, Escape or the scrim
 * closes it and focus returns to the tab.
 */
export function AccountSheet({ id, session, open, onOpenChange, opener }: AccountSheetProps) {
  const { plain } = useI18n();
  const { signOut, failed } = useSignOut();
  const { user, workspace } = session;
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="scrim" />
        <Dialog.Content
          id={id}
          className="sheet"
          aria-describedby={undefined}
          onCloseAutoFocus={(event) => {
            // Not every browser focuses a button on press, so the tab is named rather than left to whatever was focused.
            event.preventDefault();
            opener.current?.focus();
          }}
        >
          <div className="sheet-head">
            <Dialog.Title asChild>
              <h2>
                <T k="accountLabel" />
              </h2>
            </Dialog.Title>
            <Dialog.Close asChild>
              <IconButton label={plain('close')} className="sheet-close">
                <Icon name="close" />
              </IconButton>
            </Dialog.Close>
          </div>
          <div className="sheet-body">
            <div className="mcard">
              <span className="av" aria-hidden="true">
                {initialOf(user.name)}
              </span>
              <div>
                <b>
                  <LangRuns text={user.name} />
                </b>
                <small>
                  <LangRuns text={workspace.name} />
                </small>
                <small>
                  <Numeral>{user.email}</Numeral>
                </small>
              </div>
            </div>
            <Button
              className="sheet-signout"
              onClick={() => {
                void signOut();
              }}
            >
              <Icon name="signout" />
              <T k="signOut" />
            </Button>
            {failed && (
              <p role="alert" className="kiwi-err">
                <T k="signOutFailed" />
              </p>
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
