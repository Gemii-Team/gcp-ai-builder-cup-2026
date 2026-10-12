import { Icon, type IconName } from '@kiwi/ui';
import { useId, useRef, useState } from 'react';
import { useChannels, useMe } from '../../api/hooks';
import { T, useI18n } from '../../i18n';
import { useUiStore } from '../../state/ui';
import { AccountSheet } from './AccountSheet';
import { AppLink } from './AppLink';

function Pill({ icon }: { icon: IconName }) {
  return (
    <span className="tab-pill">
      <Icon name={icon} size={20} />
    </span>
  );
}

/**
 * The phone's four-item tab bar: Chat, Board and Ask are links, Me is a button that opens the account sheet. Chat leads to
 * the channel last open (the first one on a fresh start) and counts as current on any channel.
 */
export function TabBar() {
  const { plain } = useI18n();
  const session = useMe();
  const channels = useChannels();
  const lastChannelId = useUiStore((state) => state.lastChannelId);
  const [meOpen, setMeOpen] = useState(false);
  const meTab = useRef<HTMLButtonElement>(null);
  const sheetId = useId();
  const chatId = channels.data?.some((channel) => channel.id === lastChannelId) === true ? lastChannelId : channels.data?.[0]?.id;

  return (
    <nav className="tabs" aria-label={plain('tabsLabel')}>
      <AppLink to={chatId === undefined ? '/' : `/c/${chatId}`} matches="/c">
        <Pill icon="chat" />
        <T k="tabChat" />
      </AppLink>
      <AppLink to="/board">
        <Pill icon="board" />
        <T k="navBoard" />
      </AppLink>
      <AppLink to="/ask">
        <Pill icon="ask" />
        <T k="navAsk" />
      </AppLink>
      <button
        ref={meTab}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={meOpen}
        aria-controls={meOpen ? sheetId : undefined}
        onClick={() => {
          setMeOpen(true);
        }}
      >
        <Pill icon="user" />
        <T k="tabMe" />
      </button>
      {session.data !== undefined && <AccountSheet id={sheetId} session={session.data} open={meOpen} onOpenChange={setMeOpen} opener={meTab} />}
    </nav>
  );
}
