/** Someone or something the person may call with `@`: the agent, or a member of the channel. */
export type MentionTarget = { kind: 'agent' | 'user'; id: string; name: string };

/** The id of the option at `index` of the list `listId`, which the input names in `aria-activedescendant`. */
export function mentionOptionId(listId: string, index: number): string {
  return `${listId}-option-${String(index)}`;
}
