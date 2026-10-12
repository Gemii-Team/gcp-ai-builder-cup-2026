/**
 * The Reference Rule: `REF` plus a three-letter kind and a four-digit number, in Ink Teal mono. A failed attempt issues
 * none. A code is Latin whatever the interface language, so it is tagged `lang="en"`.
 */
export function RefCode({ code }: { code: string }) {
  return (
    <span lang="en" className="font-mono text-reference text-primary whitespace-nowrap">{`REF ${code}`}</span>
  );
}
