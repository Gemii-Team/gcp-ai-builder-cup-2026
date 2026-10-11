import { Icon, IconButton, LangRuns } from '@kiwi/ui';
import { useRouter } from '@tanstack/react-router';
import type { ChannelView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { AppButtonLink, AppLink } from '../shell/AppLink';

type ChannelHeaderProps = {
  channel: ChannelView;
  /** The channel's member count, once the roster has loaded. */
  memberCount: number | undefined;
};

/**
 * The 52px header: `# name` with `N members` (dropped below 1300px), and at the right Ask as the primary action and
 * Board as a secondary one, both scoped to this channel. On phones it is the 56px Back button, the title and one 44px
 * Ask icon button.
 */
export function ChannelHeader({ channel, memberCount }: ChannelHeaderProps) {
  const router = useRouter();
  const { plain } = useI18n();
  const ask = `/ask?channel=${channel.id}`;
  return (
    <header className="chead">
      <IconButton
        label={plain('back')}
        className="back"
        onClick={() => {
          router.history.back();
        }}
      >
        <Icon name="back" />
      </IconButton>
      <div className="ttl">
        <h1>
          # <LangRuns text={channel.name} />
        </h1>
        {memberCount !== undefined && (
          <p className="sub">
            <T k="membersCount" params={{ count: memberCount }} />
          </p>
        )}
      </div>
      <div className="acts">
        <AppButtonLink variant="primary" to={ask}>
          <Icon name="ask" />
          <T k="navAsk" />
        </AppButtonLink>
        <AppButtonLink to={`/board?channel=${channel.id}`}>
          <Icon name="board" />
          <T k="navBoard" />
        </AppButtonLink>
      </div>
      <AppLink to={ask} className="ib ask-ic" aria-label={plain('navAsk')}>
        <Icon name="ask" />
      </AppLink>
    </header>
  );
}
