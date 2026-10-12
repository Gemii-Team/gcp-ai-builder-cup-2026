/** The last `seq` the client has seen in each channel. Seqs are `number` here: convert API values with `toSafeNumber`. */
export type CursorStore = {
  /** The channel's last seen seq, 0 when none has been seen. */
  get: (channelId: string) => number;
  /** Records `seq` as the channel's last seen one when it is newer than the stored one; an older seq changes nothing. */
  advance: (channelId: string, seq: number) => void;
};

/** Cursors kept in memory: a page reload starts again from the history the app loads. */
export function createMemoryCursorStore(): CursorStore {
  const cursors = new Map<string, number>();
  return {
    get: (channelId) => cursors.get(channelId) ?? 0,
    advance(channelId, seq) {
      if (seq > (cursors.get(channelId) ?? 0)) {
        cursors.set(channelId, seq);
      }
    },
  };
}
