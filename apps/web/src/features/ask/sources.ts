import type { I18n } from '@kiwi/core';
import { formatClock, formatOffset, type Source } from '@kiwi/ui';
import type { CitationView } from '../../api/types';

/**
 * The name a cited source goes by in a row, in a marker's name and in the announcement: the meeting's title, the file's
 * name, and for a message `{author} in #{channel}` (the channel named from the channels the person is in; a channel with no
 * known name leaves the author alone).
 */
export function sourceName(source: Source, channelName: string | undefined, plain: I18n['plain']): string {
  if (source.kind === 'message' && channelName !== undefined && channelName !== '') {
    return plain('askMessageSource', { author: source.label, channel: channelName });
  }
  return source.label;
}

/** Where in its source a citation points, as text: the transcript offset `0:33:18`, the page `p. 4` or the message's `14:02`. Empty when it has none. */
export function sourceLocation(source: Source, plain: I18n['plain']): string {
  switch (source.kind) {
    case 'meeting':
      return formatOffset(source.offsetMs) ?? '';
    case 'file':
      return source.page === undefined ? '' : plain('askPageShort', { page: source.page });
    case 'message':
      return source.at === undefined ? '' : formatClock(source.at);
  }
}

/** A citation's source as the slip's row shows it: the same source, named for a person to read. */
export function displaySource(citation: CitationView, channelNames: ReadonlyMap<string, string>, plain: I18n['plain']): Source {
  const { source } = citation;
  return { ...source, label: sourceName(source, channelNames.get(source.channelId), plain) };
}

/** What a citation marker says it points at: the source's name, and a document's page (`ACME-contract-v3.pdf, p. 4`). */
export function markerLabel(source: Source, plain: I18n['plain']): string {
  return source.page === undefined ? source.label : `${source.label}, ${plain('askPageShort', { page: source.page })}`;
}
