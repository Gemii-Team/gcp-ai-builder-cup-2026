import { Icon, LangRuns, Mark, Numeral } from '@kiwi/ui';
import { useChannels, useMe } from '../../api/hooks';
import { T, useI18n } from '../../i18n';
import { useUiStore } from '../../state/ui';
import { AccountMenu } from './AccountMenu';
import { AppLink } from './AppLink';
import { LoadFailed } from './LoadFailed';

/**
 * The Ink Teal rail of the desktop: the workspace, the Views (Board and Ask, with `aria-current` on the current route),
 * the channels with their unread pills, the agent and the signed-in person. On phones it gives way to the tab bar.
 */
export function Rail() {
  const { plain } = useI18n();
  const session = useMe();
  const channels = useChannels();
  const readSeq = useUiStore((state) => state.readSeq);

  return (
    <nav className="rail" aria-label={plain('railLabel')}>
      <div className="rail-brand">
        <Mark size={40} />
        <div>
          <strong>K.I.W.I.</strong>
          <small>{session.data === undefined ? null : <LangRuns text={session.data.workspace.name} />}</small>
        </div>
      </div>
      <ul aria-label={plain('railViews')}>
        <li>
          <AppLink to="/board" className="ch" data-view="">
            <Icon name="board" />
            <T k="navBoard" />
          </AppLink>
        </li>
        <li>
          <AppLink to="/ask" className="ch" data-view="">
            <Icon name="ask" />
            <T k="navAsk" />
          </AppLink>
        </li>
      </ul>
      <h2>
        <T k="railChannels" />
      </h2>
      <ul>
        {channels.data?.map((channel) => {
          const read = readSeq[channel.id];
          const unread = read === undefined ? 0 : Math.max(0, channel.lastSeq - read);
          return (
            <li key={channel.id}>
              <AppLink to={`/c/${channel.id}`} className="ch" data-unread={unread > 0 || undefined}>
                <span className="h" aria-hidden="true">
                  #
                </span>
                <LangRuns text={channel.name} />
                {unread > 0 && (
                  <span className="cnt" aria-label={plain('unreadMessages', { count: unread })}>
                    <Numeral>{unread}</Numeral>
                  </span>
                )}
              </AppLink>
            </li>
          );
        })}
      </ul>
      {channels.isError && channels.data === undefined && (
        <LoadFailed
          onRetry={() => {
            void channels.refetch();
          }}
        >
          <T k="channelsLoadFailed" />
        </LoadFailed>
      )}
      <h2>
        <T k="railAgents" />
      </h2>
      <ul>
        <li>
          <span className="ch">
            <span className="dot" aria-hidden="true" />
            @assistant
            <span className="st">
              <T k="agentOnline" />
            </span>
          </span>
        </li>
      </ul>
      {session.data !== undefined && <AccountMenu session={session.data} />}
    </nav>
  );
}
