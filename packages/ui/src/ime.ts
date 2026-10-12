/**
 * Whether a key press belongs to an input method editor (a Thai or CJK IME) rather than to the screen: `isComposing`
 * while a candidate is being composed, and keyCode 229 on the Enter or Escape that Safari delivers just after the
 * composition ended. The Enter that commits a candidate and the Escape that dismisses one must neither send, save nor
 * cancel anything.
 */
export function isImeKey({ nativeEvent }: { nativeEvent: KeyboardEvent }): boolean {
  return nativeEvent.isComposing || nativeEvent.keyCode === 229;
}
