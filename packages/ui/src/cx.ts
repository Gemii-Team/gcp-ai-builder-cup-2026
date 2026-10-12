/** Joins the class names that are set; the falsy ones (`cond && 'x'`) drop out. */
export function cx(...names: (string | false | null | undefined)[]): string {
  return names.filter(Boolean).join(' ');
}
