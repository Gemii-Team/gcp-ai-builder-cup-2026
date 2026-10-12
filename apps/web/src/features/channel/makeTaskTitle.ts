import type { ChatMessage } from '../../api/types';
import { clipRunes } from '../../lib/text';

/** The longest title a task made from a message starts with, in runes. */
const MAX_TITLE_RUNES = 120;

const escapeRegExp = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/**
 * What a task made from a message is called to begin with: the message without the people it calls with `@` (and the
 * assistant, always), its whitespace collapsed, clipped to 120 runes with `…`. A message that is only a file has no words
 * to start from, so the task follows up on the file; `followUp` words that in the interface language.
 */
export function titleFromMessage(message: ChatMessage, followUp: (file: string) => string): string {
  const names = ['assistant', ...message.mentions.map((mention) => mention.label)].sort((a, b) => b.length - a.length).map(escapeRegExp);
  const words = message.body
    .replace(new RegExp(`@(?:${names.join('|')})`, 'gu'), ' ')
    .replace(/\s+/g, ' ')
    .trim();
  if (words !== '') {
    return clipRunes(words, MAX_TITLE_RUNES);
  }
  const file = message.files[0];
  return file === undefined ? '' : followUp(file.name);
}
