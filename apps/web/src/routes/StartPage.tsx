import { Pending } from '@kiwi/ui';
import { useNavigate } from '@tanstack/react-router';
import { useEffect } from 'react';
import { useChannels } from '../api/hooks';
import { T, useI18n } from '../i18n';
import { useUiStore } from '../state/ui';

/** Where `/` leads: to the channel last open, or the first one the person is in. */
export function StartPage() {
  const { plain } = useI18n();
  const navigate = useNavigate();
  const channels = useChannels();
  const lastChannelId = useUiStore((state) => state.lastChannelId);
  const target = channels.data?.find((channel) => channel.id === lastChannelId)?.id ?? channels.data?.[0]?.id;

  useEffect(() => {
    if (target !== undefined) {
      void navigate({ to: '/c/$channelId', params: { channelId: target }, replace: true });
    }
  }, [target, navigate]);

  return (
    <main className="main">
      <div className="notice-page">{channels.data?.length === 0 ? <T k="noChannels" /> : <Pending label={plain('loading')} />}</div>
    </main>
  );
}
