import { Icon, LangRuns, Menu, MenuItem, Numeral } from '@kiwi/ui';
import { useRef, useState, type KeyboardEvent } from 'react';
import type { SessionView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { initialOf } from '../../lib/text';
import { useSignOut } from './useSignOut';

/**
 * The rail's foot: the signed-in person as a button that opens the account menu directly above it, as wide as the
 * button, with the address and Sign out. It opens by click or Arrow key and puts focus on Sign out; Escape closes it
 * and returns focus to the button.
 */
export function AccountMenu({ session }: { session: SessionView }) {
  const { plain } = useI18n();
  const [open, setOpen] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const { signOut, failed } = useSignOut();
  const { user, workspace } = session;

  function onKeyDown(event: KeyboardEvent<HTMLButtonElement>) {
    if ((event.key === 'ArrowUp' || event.key === 'ArrowDown') && !open) {
      event.preventDefault();
      setOpen(true);
    }
  }

  return (
    <div className="mewrap">
      <button
        ref={button}
        type="button"
        className="me"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => {
          setOpen(!open);
        }}
        onKeyDown={onKeyDown}
      >
        <span className="av" aria-hidden="true">
          {initialOf(user.name)}
        </span>
        <span>
          <b>
            <LangRuns text={user.name} />
          </b>
          <small>
            <LangRuns text={workspace.name} />
          </small>
        </span>
        <Icon name="chevron-up" className="chv" />
      </button>
      <Menu
        label={plain('accountLabel')}
        open={open}
        onClose={() => {
          setOpen(false);
        }}
        anchorRef={button}
        placement="above"
      >
        <p className="menu-h">
          <T k="signedInAs" /> <Numeral className="n">{user.email}</Numeral>
        </p>
        <MenuItem
          icon="signout"
          onSelect={() => {
            void signOut();
          }}
        >
          <T k="signOut" />
        </MenuItem>
      </Menu>
      {failed && (
        <p role="alert" className="me-error">
          <Icon name="warning" />
          <T k="signOutFailed" />
        </p>
      )}
    </div>
  );
}
