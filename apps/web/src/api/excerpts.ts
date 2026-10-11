import type { QueryClient } from '@tanstack/react-query';
import { toSafeNumber } from '@kiwi/core';
import { formatClock, formatDay, type Excerpt, type Source, type UiLang } from '@kiwi/ui';
import type { Api } from './client';
import { fileQuery, keys, type MessagesData } from './queries';
import type { ChannelView } from './types';

/** How many transcript entries on each side of the one a source points at the in-place excerpt shows. */
const TRANSCRIPT_RADIUS = 2;

/** A message's excerpt names a day only when the message is not from today. */
function dayOf(at: string, lang: UiLang): string | undefined {
  return formatDay(at, lang) === formatDay(new Date(), lang) ? undefined : formatDay(at, lang);
}

/** The text of the message a source points at: from the cached stream when it holds it, else the one `ListMessages` row. */
async function messageText(api: Api, queryClient: QueryClient, source: Source): Promise<string> {
  const cached = queryClient.getQueryData<MessagesData>(keys.messages(source.channelId))?.messages.find((m) => m.id === source.id);
  if (cached !== undefined) {
    return cached.body;
  }
  if (source.seq === undefined) {
    throw new Error('A message source needs its seq to be opened.');
  }
  // The page holds messages below `before_seq`, newest first: one past the seq makes the message itself the first row.
  const { messages } = await api.chat.listMessages({ channelId: source.channelId, beforeSeq: BigInt(source.seq + 1), limit: 1 });
  const message = messages.find((m) => m.id === source.id);
  if (message === undefined) {
    throw new Error('The message of this source is not in its channel.');
  }
  return message.body;
}

/**
 * The `load` of an `ExcerptProvider`: what opens in place when an offset, a page or a message time is pressed. A
 * transcript comes from `GetTranscriptExcerpt` around the entry the source names, a document page from the quote the
 * source carries (the file's REF and page count come from `GetFile`) and a message from the stream or `ListMessages`.
 */
export function createExcerptLoader(api: Api, queryClient: QueryClient, lang: UiLang): (source: Source) => Promise<Excerpt> {
  return async (source) => {
    switch (source.kind) {
      case 'meeting': {
        const { excerpt } = await api.meetings.getTranscriptExcerpt({
          meetingId: source.id,
          entryIdx: source.entryIdx ?? 0,
          radius: TRANSCRIPT_RADIUS,
        });
        if (excerpt === undefined) {
          throw new Error('GetTranscriptExcerpt answered without an excerpt.');
        }
        return {
          kind: 'transcript',
          title: excerpt.title === '' ? source.label : excerpt.title,
          offsetMs: source.offsetMs ?? null,
          lines: excerpt.entries.map((entry) => ({
            speaker: entry.speaker ?? null,
            offsetMs: entry.startMs === undefined ? null : toSafeNumber(entry.startMs),
            text: entry.text,
          })),
        };
      }
      case 'file': {
        const file = await queryClient.fetchQuery({ ...fileQuery(api, source.id), staleTime: 60_000 });
        return {
          kind: 'document',
          file: source.label === '' ? file.name : source.label,
          ref: file.ref,
          page: source.page ?? 1,
          ...(file.pages > 0 ? { pages: file.pages } : {}),
          ...(source.quote === undefined ? {} : { quote: source.quote }),
        };
      }
      case 'message': {
        const channel = queryClient.getQueryData<ChannelView[]>(keys.channels())?.find((c) => c.id === source.channelId);
        const at = source.at ?? new Date(0).toISOString();
        const day = dayOf(at, lang);
        return {
          kind: 'message',
          channel: channel?.name ?? '',
          ...(day === undefined ? {} : { day }),
          time: formatClock(at),
          author: source.label,
          text: await messageText(api, queryClient, source),
        };
      }
    }
  };
}
