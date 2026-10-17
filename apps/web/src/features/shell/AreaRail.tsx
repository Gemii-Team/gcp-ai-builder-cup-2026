import { Icon, Mark } from '@kiwi/ui';
import { useRouterState } from '@tanstack/react-router';
import { useChannels } from '../../api/hooks';
import { T, useI18n } from '../../i18n';
import { useUiStore } from '../../state/ui';
import { AppLink } from './AppLink';
import { AREAS, areaOf, type AreaId } from './areas';

/**
 * The thin left rail of the desktop, Asana-style: the K.I.W.I. mark and the app areas (Chat, Workspace, Calendar,
 * Agents). The area the current route belongs to is lit; picking one opens that area and its inner sidebar. Chat leads
 * to the channel last open.
 */
export function AreaRail() {
  const { plain } = useI18n();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const active = areaOf(pathname);
  const channels = useChannels();
  const lastChannelId = useUiStore((state) => state.lastChannelId);
  const chatId = channels.data?.some((channel) => channel.id === lastChannelId) === true ? lastChannelId : channels.data?.[0]?.id;

  function to(id: AreaId): string {
    switch (id) {
      case 'chat':
        return chatId === undefined ? '/' : `/c/${chatId}`;
      case 'work':
        return '/board';
      case 'calendar':
        return '/calendar';
      case 'agents':
        return '/ask';
    }
  }

  return (
    <nav className="arearail" aria-label={plain('areaRailLabel')}>
      <AppLink to={chatId === undefined ? '/' : `/c/${chatId}`} className="arearail-brand" aria-label="K.I.W.I.">
        <Mark size={40} />
      </AppLink>
      <ul>
        {AREAS.map((area) => (
          <li key={area.id}>
            <AppLink to={to(area.id)} className="area" data-active={area.id === active || undefined}>
              <Icon name={area.icon} size={22} />
              <span className="area-lbl">
                <T k={area.label} />
              </span>
            </AppLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}
