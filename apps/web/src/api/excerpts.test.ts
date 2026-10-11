import { create } from '@bufbuild/protobuf';
import { GetFileResponseSchema, ListMessagesResponseSchema } from '@kiwi/api-client';
import type { Source } from '@kiwi/ui';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { at, file, message } from '../testing/fixtures';
import { createTestQueryClient } from '../testing/render';
import { createStubApi } from '../testing/stubApi';
import { createExcerptLoader } from './excerpts';
import { keys } from './queries';
import type { ChannelView } from './types';

// "Now" is 14:40 on the demo day, Tue 13 Oct 2026 in Bangkok.
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(at('14:40'));
});

afterEach(() => {
  vi.useRealTimers();
});

const CHANNELS: ChannelView[] = [{ id: 'ch-q4', name: 'q4-launch', kind: 'public', lastSeq: 5 }];

function loader() {
  const api = createStubApi();
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(keys.channels(), CHANNELS);
  return { api, queryClient, load: createExcerptLoader(api, queryClient, 'en') };
}

const messageSource: Source = { kind: 'message', id: 'msg-41', label: 'Arthit K.', channelId: 'ch-q4', seq: 41, at: at('14:02').toISOString() };

describe('the excerpt loader: a message source', () => {
  it('reads the message from ListMessages at its seq and names its channel, author and time', async () => {
    const { api, load } = loader();
    api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: [message({ seq: 41, body: 'ยืนยันแล้วครับ' })] }));

    const excerpt = await load(messageSource);

    // The page holds the messages below `before_seq`, newest first: one past the seq makes the message the first row.
    expect(api.chat.listMessages).toHaveBeenCalledWith({ channelId: 'ch-q4', beforeSeq: 42n, limit: 1 });
    expect(excerpt).toEqual({ kind: 'message', channel: 'q4-launch', time: '14:02', author: 'Arthit K.', text: 'ยืนยันแล้วครับ' });
  });

  it('names the day when the message is not from today', async () => {
    const { api, load } = loader();
    api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: [message({ seq: 41, body: 'Pilot week starts 3 Nov.' })] }));

    const excerpt = await load({ ...messageSource, at: at('17:05', '2026-10-12').toISOString() });

    expect(excerpt).toMatchObject({ kind: 'message', day: 'Mon 12 Oct', time: '17:05' });
  });

  it('takes the text from the cached stream and asks the server for nothing', async () => {
    const { api, queryClient, load } = loader();
    queryClient.setQueryData(keys.messages('ch-q4'), {
      hasEarlier: false,
      messages: [{ id: 'msg-41', channelId: 'ch-q4', seq: 41, body: 'From the stream' }],
    });

    const excerpt = await load(messageSource);

    expect(api.chat.listMessages).not.toHaveBeenCalled();
    expect(excerpt).toMatchObject({ kind: 'message', text: 'From the stream' });
  });

  it('leaves out the time, and the day, when the source has none, instead of the epoch', async () => {
    const { api, load } = loader();
    api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: [message({ seq: 41, body: 'No time known' })] }));
    const untimed: Source = { kind: 'message', id: 'msg-41', label: 'Arthit K.', channelId: 'ch-q4', seq: 41 };

    const excerpt = await load(untimed);

    expect(excerpt).toEqual({ kind: 'message', channel: 'q4-launch', author: 'Arthit K.', text: 'No time known' });
  });

  it('rejects when the message is not in its channel, so the excerpt shows its error', async () => {
    const { api, load } = loader();
    api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: [] }));

    await expect(load(messageSource)).rejects.toThrow('not in its channel');
  });
});

describe('the excerpt loader: a document source', () => {
  const fileSource: Source = { kind: 'file', id: 'file-1', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4, quote: '4.1 Term. This Agreement ends on 31 December 2026.' };

  it('shows the quote the task carries with the page and the REF and page count GetFile gives', async () => {
    const { api, load } = loader();
    api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ id: 'file-1', name: 'ACME-contract-v3.pdf', ref: 'DOC-0371', pages: 18 }) }));

    const excerpt = await load(fileSource);

    expect(api.files.getFile).toHaveBeenCalledTimes(1);
    expect(excerpt).toEqual({
      kind: 'document',
      file: 'ACME-contract-v3.pdf',
      ref: 'DOC-0371',
      page: 4,
      pages: 18,
      quote: '4.1 Term. This Agreement ends on 31 December 2026.',
    });
  });

  it('leaves out the passage when the task has no quote, and the page count when the file has none', async () => {
    const { api, load } = loader();
    api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ id: 'file-1', ref: 'DOC-0371', pages: 0 }) }));
    const unquoted: Source = { kind: 'file', id: 'file-1', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 };

    const excerpt = await load(unquoted);

    expect(excerpt).toEqual({ kind: 'document', file: 'ACME-contract-v3.pdf', ref: 'DOC-0371', page: 4 });
  });

  it('reads the file through the cache the lane of its source shares, so it asks once', async () => {
    const { api, queryClient, load } = loader();
    api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ id: 'file-1', ref: 'DOC-0371', pages: 18 }) }));

    await load(fileSource);
    await load(fileSource);

    expect(api.files.getFile).toHaveBeenCalledTimes(1);
    expect(queryClient.getQueryData(keys.file('file-1'))).toMatchObject({ ref: 'DOC-0371', pages: 18 });
  });
});

