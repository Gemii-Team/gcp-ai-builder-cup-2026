import '../shell/screen.css';
import './board.css';
import { Button, DraftsBar, ExcerptProvider, formatDay, Icon, IconButton, Pending, type SealedTask } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { useCallback, useId, useMemo, useRef, useState } from 'react';
import { useApi } from '../../api/client';
import { createExcerptLoader } from '../../api/excerpts';
import { useChannels, useMe, useUsers } from '../../api/hooks';
import type { TaskView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { useAnnouncer } from '../../lib/useAnnouncer';
import { Glide } from '../../lib/Glide';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { useUiStore } from '../../state/ui';
import { AppButtonLink, AppLink } from '../shell/AppLink';
import { DraftsSheet } from '../shell/DraftsSheet';
import { LoadFailed } from '../shell/LoadFailed';
import { ColumnHeads } from './ColumnHeads';
import { DraftsBand } from './DraftsBand';
import { FilterBar } from './FilterBar';
import { Lane } from './Lane';
import { NewTask } from './NewTask';
import { TaskCard, type Landing } from './TaskCard';
import { cellsOf, EMPTY_KEY, matchesFilters, NO_FILTERS, STATUS_KEY, useBoardData, type BoardFilters, type Status } from './useBoardData';

/** A draft this person confirmed and has not moved since: its card in To do stays the sealed slip. */
type Confirmed = { id: string; landing: Landing; sealed: SealedTask };

type BoardScreenProps = {
  /** `?channel=<id>` of the route: the Channel filter starts on it. */
  initialChannelId: string | null;
};

/**
 * What the route `/board` shows: every task of the person's channels filed under the source it came from, each source a
 * lane with a cell per status, with the AI's drafts waiting in a band above the lanes until a person seals them. The
 * tasks are the one workspace-wide list in the query cache, so a move, a confirmation, a hand-made task or a realtime
 * `task.changed` shows at once. Below 1024px the column heads give way to a status control, a lane shows one status and
 * the band is the bottom sheet opened from the drafts bar.
 */
export function BoardScreen({ initialChannelId }: BoardScreenProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const router = useRouter();
  const { lang, plain } = useI18n();
  const phone = useMediaQuery('(max-width: 1023px)');
  const channels = useChannels();
  const users = useUsers();
  const me = useMe().data;
  const [requested, setFilters] = useState<BoardFilters>({ channelId: initialChannelId, sourceType: null });
  const [status, setStatus] = useState<Status>('todo');
  const [confirmed, setConfirmed] = useState<Confirmed[]>([]);
  const [formOpen, setFormOpen] = useState(false);
  const [announcement, say] = useAnnouncer();
  const [focusId, setFocusId] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const sheetOpen = useUiStore((state) => state.draftsSheetOpen);
  const setSheetOpen = useUiStore((state) => state.setDraftsSheetOpen);
  const formId = useId();
  const sheetId = useId();
  const opener = useRef<HTMLElement | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const filterBar = useRef<HTMLDivElement>(null);
  const bar = useRef<HTMLButtonElement>(null);
  const load = useMemo(() => createExcerptLoader(api, queryClient, lang), [api, queryClient, lang]);

  // A channel the person is not in (a stale link) filters nothing.
  const filters = useMemo(
    () => (requested.channelId === null || channels.data === undefined || channels.data.some((channel) => channel.id === requested.channelId) ? requested : { ...requested, channelId: null }),
    [requested, channels.data],
  );
  const pinned = useMemo(() => confirmed.map((entry) => entry.id), [confirmed]);
  const landings = useMemo(() => new Map(confirmed.map((entry) => [entry.id, entry.landing])), [confirmed]);
  const { board, isPending, isError, refetch } = useBoardData(filters, pinned);

  // Which cell each card is in: a card that changes cell glides from its old place to its new one (a move, a confirm, a
  // change another client made).
  const cells = useMemo(() => cellsOf(board), [board]);

  const channelNames = useMemo(() => new Map((channels.data ?? []).map((channel) => [channel.id, channel.name])), [channels.data]);
  const userNames = useMemo(() => new Map((users.data ?? []).map((user) => [user.id, user.name])), [users.data]);
  const now = new Date();

  /** The title of the lane a task is filed under, as text. */
  function laneTitle(task: TaskView): string {
    if (task.source === null) {
      return plain('sourceByHand');
    }
    return task.source.kind === 'message' ? plain('messagesIn', { channel: channelNames.get(task.channelId) ?? '' }) : task.source.label;
  }

  /** A task the person must see is not hidden by a filter: the filters clear. Whether they did is for the announcement. */
  function clearIfHidden(task: TaskView): boolean {
    const hidden = !matchesFilters(task, filters);
    if (hidden) {
      setFilters(NO_FILTERS);
    }
    return hidden;
  }

  function onConfirmed(task: TaskView) {
    const { source } = task;
    if (source === null) {
      return;
    }
    const landing: Landing = { confirmedBy: me?.user.name ?? '', confirmedAt: new Date().toISOString() };
    const ownerName = task.assigneeId === null ? undefined : userNames.get(task.assigneeId);
    const sealed: SealedTask = {
      id: task.id,
      ref: task.ref,
      title: task.title,
      source,
      ...landing,
      note: plain('taskLandedNote'),
      ...(ownerName === undefined ? {} : { ownerName }),
      ...(task.dueAt === null ? {} : { dueLabel: formatDay(task.dueAt, lang) }),
    };
    setConfirmed((list) => [{ id: task.id, landing, sealed }, ...list.filter((entry) => entry.id !== task.id)]);
    const cleared = clearIfHidden(task);
    if (phone) {
      setStatus('todo');
    }
    say(`${plain('taskLanded', { lane: laneTitle(task) })}${cleared ? ` ${plain('filtersCleared')}` : ''}`);
  }

  function onCreated(task: TaskView) {
    const cleared = clearIfHidden(task);
    closeForm(false);
    if (phone) {
      setStatus('todo');
    }
    setFocusId(task.id);
    say(`${plain('taskCreatedByHand', { code: task.ref })}${cleared ? ` ${plain('filtersCleared')}` : ''}`);
  }

  function toggleForm(button: HTMLElement) {
    if (formOpen) {
      closeForm(true);
      return;
    }
    opener.current = button;
    setFormOpen(true);
    if (scroller.current !== null) {
      scroller.current.scrollTop = 0;
    }
    say(plain('newTaskOpened'));
  }

  function closeForm(restoreFocus: boolean) {
    setFormOpen(false);
    const button = opener.current;
    opener.current = null;
    if (restoreFocus) {
      button?.focus();
    }
  }

  const onMoved = useCallback(
    (task: TaskView, to: Status) => {
      setFailure(null);
      // The first move turns a just-confirmed card into a plain one.
      setConfirmed((list) => (list.some((entry) => entry.id === task.id) ? list.filter((entry) => entry.id !== task.id) : list));
      if (phone) {
        // The card leaves the status on show, so focus goes to the status control, which holds the place.
        filterBar.current?.querySelector<HTMLElement>('[aria-pressed="true"]')?.focus();
      } else {
        setFocusId(task.id);
      }
      say(plain('taskMoved', { code: task.ref, status: plain(STATUS_KEY[to]) }));
    },
    [phone, plain, say],
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
  const lanes = board === undefined ? [] : board.lanes.filter((lane) => !phone || lane.cells[status].length > 0);
  const noChannels = (channels.data?.length ?? 0) === 0;
  // The sealed card lands in its lane; the list of those just confirmed is the phone sheet's, where Move would be.
  const band =
    board === undefined ? null : (
      <DraftsBand drafts={board.drafts} sealed={phone ? confirmed.map((entry) => entry.sealed) : []} counts={board.counts} onConfirmed={onConfirmed} />
    );

  return (
    <Glide root={scroller} cells={cells}>
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
              <Button
                aria-expanded={formOpen}
                aria-controls={formId}
                disabled={noChannels}
                onClick={(event) => {
                  toggleForm(event.currentTarget);
                }}
              >
                <Icon name="task-add" />
                <T k="newTask" />
              </Button>
              <AppButtonLink to="/ask">
                <Icon name="ask" />
                <T k="navAsk" />
              </AppButtonLink>
            </div>
            <IconButton
              label={plain('newTask')}
              className="ask-ic"
              aria-expanded={formOpen}
              aria-controls={formId}
              disabled={noChannels}
              onClick={(event) => {
                toggleForm(event.currentTarget);
              }}
            >
              <Icon name="task-add" />
            </IconButton>
            <AppLink to="/ask" className="ib ask-ic" aria-label={plain('navAsk')}>
              <Icon name="ask" />
            </AppLink>
          </header>
          <FilterBar
            ref={filterBar}
            channels={channels.data ?? []}
            filters={filters}
            onChange={setFilters}
            phone={phone}
            status={status}
            counts={board?.counts ?? { todo: 0, doing: 0, done: 0 }}
            onStatus={setStatus}
          />
          {failure !== null && (
            <p className="notice bd-notice" role="alert">
              {failure}
            </p>
          )}
          <div ref={scroller} className="bd-scroller" role="region" aria-label={plain('boardTasks')} tabIndex={0}>
            {formOpen && (
              <NewTask
                id={formId}
                channels={channels.data ?? []}
                filteredChannelId={filters.channelId}
                onCreated={onCreated}
                onCancel={() => {
                  closeForm(true);
                }}
              />
            )}
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
                {!phone && band}
                {!phone && <ColumnHeads counts={board.counts} />}
                <div className="bd-lanes">
                  {lanes.map((lane) => (
                    <Lane
                      key={lane.key}
                      lane={lane}
                      channelName={channelNames.get(lane.channelId ?? '') ?? ''}
                      only={phone ? status : null}
                      renderCard={(task, owner) => (
                        <TaskCard
                          key={task.id}
                          task={task}
                          laneKind={owner.kind}
                          channelName={channelNames.get(task.channelId) ?? ''}
                          ownerName={task.assigneeId === null ? undefined : userNames.get(task.assigneeId)}
                          creatorName={userNames.get(task.createdById)}
                          now={now}
                          landing={task.status === 'todo' ? landings.get(task.id) : undefined}
                          focus={focusId === task.id}
                          onFocused={onFocused}
                          onMoved={onMoved}
                          onMoveFailed={onMoveFailed}
                        />
                      )}
                    />
                  ))}
                </div>
                {phone && shown > 0 && lanes.length === 0 && (
                  <p className="bd-cell-empty">
                    <T k={EMPTY_KEY[status]} />
                  </p>
                )}
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
          {phone && board !== undefined && (
            <DraftsBar
              ref={bar}
              count={board.drafts.length}
              expanded={sheetOpen}
              controls={sheetId}
              onPress={() => {
                setSheetOpen(true);
              }}
            />
          )}
          <p role="status" className="sr-only">
            {announcement}
          </p>
        </main>
      </div>
      {phone && (
        <DraftsSheet id={sheetId} open={sheetOpen} onOpenChange={setSheetOpen} opener={bar}>
          {band}
        </DraftsSheet>
      )}
    </ExcerptProvider>
    </Glide>
  );
}
