import { Icon, LangRuns, Numeral } from '@kiwi/ui';
import { useRouterState } from '@tanstack/react-router';
import { useChannels, useMe } from '../../api/hooks';
import { T, useI18n } from '../../i18n';
import { useUiStore } from '../../state/ui';
import { AccountMenu } from './AccountMenu';
import { AppLink } from './AppLink';
import { LoadFailed } from './LoadFailed';
import { areaOf, type AreaId } from './areas';

const AREA_TITLE: Record<AreaId, 'areaChat' | 'areaWork' | 'areaCalendar' | 'areaAgents'> = {
  chat: 'areaChat',
  work: 'areaWork',
  calendar: 'areaCalendar',
  agents: 'areaAgents',
};

/**
 * The contextual sidebar beside the thin rail, Asana-style: it shows the active area's things — the channels of Chat,
 * the tools of Workspace, the Calendar, or the agents — and the signed-in person at its foot.
 */
export function InnerSidebar() {
  const { plain } = useI18n();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const area = areaOf(pathname);
  const session = useMe();

  return (
    <div className="inner">
      <h2 className="inner-h">
        <T k={AREA_TITLE[area]} />
      </h2>
      <nav className="inner-nav" aria-label={plain('railLabel')}>
        {area === 'chat' && <ChatNav />}
        {area === 'work' && <WorkNav />}
        {area === 'calendar' && <CalendarNav />}
        {area === 'agents' && <AgentsNav />}
      </nav>
      {session.data !== undefined && <AccountMenu session={session.data} />}
    </div>
  );
}

function ChatNav() {
  const { plain } = useI18n();
  const channels = useChannels();
  const readSeq = useUiStore((state) => state.readSeq);
  return (
    <ul className="inner-list">
      {channels.data?.map((channel) => {
        const read = readSeq[channel.id];
        const unread = read === undefined ? 0 : Math.max(0, channel.lastSeq - read);
        return (
          <li key={channel.id}>
            <AppLink to={`/c/${channel.id}`} className="ich" data-unread={unread > 0 || undefined}>
              <span className="ich-h" aria-hidden="true">
                #
              </span>
              <LangRuns text={channel.name} />
              {unread > 0 && (
                <span className="ich-cnt" aria-label={plain('unreadMessages', { count: unread })}>
                  <Numeral>{unread}</Numeral>
                </span>
              )}
            </AppLink>
          </li>
        );
      })}
      {channels.isError && channels.data === undefined && (
        <li>
          <LoadFailed
            onRetry={() => {
              void channels.refetch();
            }}
          >
            <T k="channelsLoadFailed" />
          </LoadFailed>
        </li>
      )}
    </ul>
  );
}

function WorkNav() {
  return (
    <ul className="inner-list">
      <li>
        <AppLink to="/board" className="ich">
          <Icon name="board" />
          <T k="navBoard" />
        </AppLink>
      </li>
      <li>
        <AppLink to="/meetings" className="ich">
          <Icon name="video" />
          <T k="navMeetings" />
        </AppLink>
      </li>
    </ul>
  );
}

function CalendarNav() {
  return (
    <ul className="inner-list">
      <li>
        <AppLink to="/calendar" className="ich">
          <Icon name="clock" />
          <T k="navCalendar" />
        </AppLink>
      </li>
    </ul>
  );
}

function AgentsNav() {
  return (
    <ul className="inner-list">
      <li>
        <AppLink to="/ask" className="ich">
          <Icon name="ask" />
          <T k="navAsk" />
        </AppLink>
      </li>
      <li>
        <span className="ich ich-agent">
          <span className="dot" aria-hidden="true" />
          @assistant
          <span className="ich-st">
            <T k="agentOnline" />
          </span>
        </span>
      </li>
    </ul>
  );
}
