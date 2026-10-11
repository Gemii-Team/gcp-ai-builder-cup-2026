import './board.css';
import { Button, ExcerptProvider, Icon, IconButton, Pending } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { useCallback, useMemo, useState } from 'react';
import { useApi } from '../../api/client';
import { createExcerptLoader } from '../../api/excerpts';
import { useChannels, useUsers } from '../../api/hooks';
import type { TaskView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { useAnnouncer } from '../../lib/useAnnouncer';
import { AppButtonLink, AppLink } from '../shell/AppLink';
import { LoadFailed } from '../shell/LoadFailed';
import { ColumnHeads } from './ColumnHeads';
import { FilterBar } from './FilterBar';
import { Lane } from './Lane';
import { TaskCard } from './TaskCard';
import { NO_FILTERS, STATUS_KEY, useBoardData, type BoardFilters, type Status } from './useBoardData';

type BoardScreenProps = {
  /** `?channel=<id>` of the route: the Channel filter starts on it. */
  initialChannelId: string | null;
};

/**
 * What the route `/board` shows: every task of the person's channels filed under the source it came from, each source a
 * lane with a cell per status. The tasks are the one workspace-wide list in the query cache, so a move, a confirmation
 * or a realtime `task.changed` shows at once.
 */
export function BoardScreen({ initialChannelId }: BoardScreenProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const router = useRouter();
  const { lang, plain } = useI18n();
  const channels = useChannels();
  const users = useUsers();
  const [requested, setFilters] = useState<BoardFilters>({ channelId: initialChannelId, sourceType: null });
  const [announcement, say] = useAnnouncer();
  const [focusId, setFocusId] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const load = useMemo(() => createExcerptLoader(api, queryClient, lang), [api, queryClient, lang]);

  // A channel the person is not in (a stale link) filters nothing.
  const filters = useMemo(
    () => (requested.channelId === null || channels.data === undefined || channels.data.some((channel) => channel.id === requested.channelId) ? requested : { ...requested, channelId: null }),
    [requested, channels.data],
  );
  const { board, isPending, isError, refetch } = useBoardData(filters);

  const channelNames = useMemo(() => new Map((channels.data ?? []).map((channel) => [channel.id, channel.name])), [channels.data]);
  const userNames = useMemo(() => new Map((users.data ?? []).map((user) => [user.id, user.name])), [users.data]);
  const now = new Date();

  const onMoved = useCallback(
    (task: TaskView, to: Status) => {
      setFailure(null);
      setFocusId(task.id);
      say(plain('taskMoved', { code: task.ref, status: plain(STATUS_KEY[to]) }));
    },
    [plain, say],
  );
  const onMoveFailed = useCallback(
    (task: TaskView) => {
      setFailure(plain('taskMoveFailed', { code: task.ref }));
    },
    [plain],
  );
  const onFocused = useCallback(() => {
    setFocusId(null);
  }, []);

  const filtered = filters.channelId !== null || filters.sourceType !== null;
  const shown = board === undefined ? 0 : board.counts.todo + board.counts.doing + board.counts.done;

  return (
    <ExcerptProvider load={load}>
      <div className="bd">
        <main className="main">
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
                <T k="navBoard" />
              </h1>
              {channels.data !== undefined && (
                <p className="sub">
                  <T k="boardSubtitle" params={{ count: channels.data.length }} />
                </p>
              )}
            </div>
            <div className="acts">
              <AppButtonLink to="/ask">
                <Icon name="ask" />
                <T k="navAsk" />
              </AppButtonLink>
            </div>
            <AppLink to="/ask" className="ib ask-ic" aria-label={plain('navAsk')}>
              <Icon name="ask" />
            </AppLink>
          </header>
          <FilterBar channels={channels.data ?? []} filters={filters} onChange={setFilters} />
          {failure !== null && (
            <p className="notice bd-notice" role="alert">
              {failure}
            </p>
          )}
          <div className="bd-scroller" role="region" aria-label={plain('boardTasks')} tabIndex={0}>
            {isPending && <Pending label={plain('loading')} />}
            {isError && board === undefined && (
              <LoadFailed
                onRetry={() => {
                  void refetch();
                }}
              >
                <T k="boardLoadFailed" />
              </LoadFailed>
            )}
            {board !== undefined && (
              <>
                <ColumnHeads counts={board.counts} />
                <div className="bd-lanes">
                  {board.lanes.map((lane) => (
                    <Lane
                      key={lane.key}
                      lane={lane}
                      channelName={channelNames.get(lane.channelId ?? '') ?? ''}
                      only={null}
                      renderCard={(task, owner) => (
                        <TaskCard
                          key={task.id}
                          task={task}
                          laneKind={owner.kind}
                          channelName={channelNames.get(task.channelId) ?? ''}
                          ownerName={task.assigneeId === null ? undefined : userNames.get(task.assigneeId)}
                          creatorName={userNames.get(task.createdById)}
                          now={now}
                          landing={undefined}
                          focus={focusId === task.id}
                          onFocused={onFocused}
                          onMoved={onMoved}
                          onMoveFailed={onMoveFailed}
                        />
                      )}
                    />
                  ))}
                </div>
                {shown === 0 && (
                  <div className="bd-nomatch">
                    <h3>
                      <T k={filtered ? 'noMatch' : 'boardEmpty'} />
                    </h3>
                    <p>
                      <T k={filtered ? 'noMatchHint' : 'boardEmptyHint'} />
                    </p>
                    {filtered && (
                      <Button
                        onClick={() => {
                          setFilters(NO_FILTERS);
                        }}
                      >
                        <T k="clearFilters" />
                      </Button>
                    )}
                  </div>
                )}
              </>
            )}
          </div>
          <p role="status" className="sr-only">
            {announcement}
          </p>
        </main>
      </div>
    </ExcerptProvider>
  );
}
