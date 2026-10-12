import { useCallback, useState } from 'react';

/**
 * Which message has its Make a task form open (one at a time: opening another closes the first), which made task takes
 * focus, and what the live region says. Closing a form without making a task returns focus to the action that opened it.
 */
export function useMakeTask() {
  const [openId, setOpenId] = useState<string | null>(null);
  const [focusTaskId, setFocusTaskId] = useState<string | null>(null);
  const [announcement, setAnnouncement] = useState('');

  const toggle = useCallback((messageId: string) => {
    setOpenId((current) => (current === messageId ? null : messageId));
    setFocusTaskId(null);
  }, []);

  const cancel = useCallback((messageId: string) => {
    setOpenId(null);
    document.querySelector<HTMLElement>(`[data-message-id="${CSS.escape(messageId)}"] .mact`)?.focus();
  }, []);

  /** The task exists: its form gives way to its card, which takes focus, and the result is announced. */
  const made = useCallback((taskId: string, text: string) => {
    setOpenId(null);
    setFocusTaskId(taskId);
    setAnnouncement(text);
  }, []);

  return { openId, focusTaskId, announcement, toggle, cancel, made };
}
