import './shell.css';
import { Outlet } from '@tanstack/react-router';
import type { RealtimeClient } from '@kiwi/core';
import { useEffect } from 'react';
import { useChannels } from '../../api/hooks';
import { RealtimeProvider } from '../../realtime/provider';
import { useUiStore } from '../../state/ui';
import { useSession } from '../auth/useSession';
import { AreaRail } from './AreaRail';
import { InnerSidebar } from './InnerSidebar';
import { TabBar } from './TabBar';

/**
 * A channel nobody has read yet starts at its head: the pill counts only what arrives after the person first saw the
 * channel list, not the whole history of every channel they are in.
 */
function useFirstSightOfChannels() {
  const channels = useChannels();
  const readSeq = useUiStore((state) => state.readSeq);
  const markRead = useUiStore((state) => state.markRead);
  useEffect(() => {
    for (const channel of channels.data ?? []) {
      if (readSeq[channel.id] === undefined) {
        markRead(channel.id, channel.lastSeq);
      }
    }
  }, [channels.data, readSeq, markRead]);
}

/**
 * The signed-in shell around every route that has a rail: the grid `rail | main`, the live connection, the phone's tab
 * bar and the demo label, which is pinned on every screen because every message, file and task here is synthetic.
 */
export function AppShell({ client }: { /** The live connection; tests pass a fake, the app builds its own. */ client?: RealtimeClient }) {
  useSession();
  useFirstSightOfChannels();
  return (
    <RealtimeProvider client={client}>
      <div className="shell">
        <p className="demo-tag">
          <span lang="en">Demo data</span> · <span lang="th">ข้อมูลตัวอย่าง</span>
        </p>
        <div className="app">
          <AreaRail />
          <InnerSidebar />
          <Outlet />
        </div>
        <TabBar />
      </div>
    </RealtimeProvider>
  );
}
