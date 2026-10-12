/** The files the server takes, by extension: the attachments (PDF, PNG, JPEG, text, Markdown, CSV) and a WAV recording. */
const BY_EXTENSION: Record<string, string> = {
  pdf: 'application/pdf',
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  txt: 'text/plain',
  md: 'text/markdown',
  csv: 'text/csv',
  wav: 'audio/wav',
};

/**
 * The MIME type to send for a file. A browser that gives none (some do for `.md` and `.csv`) is helped for the extensions
 * the server takes and no others: any other file is sent as the browser gave it, so the server's answer is about the file
 * the person chose, not about a type guessed for it.
 */
export function mimeOf({ name, type }: { name: string; type: string }): string {
  if (type !== '') {
    return type;
  }
  const extension = /^.+\.([^./\\]+)$/.exec(name)?.[1]?.toLowerCase();
  return extension !== undefined && Object.hasOwn(BY_EXTENSION, extension) ? BY_EXTENSION[extension] : '';
}
