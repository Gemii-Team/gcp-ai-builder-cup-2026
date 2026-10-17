/** The K.I.W.I. palette (packages/ui/src/theme.css), so the cards and captions match the app on screen. */
export const color = {
  primary: '#0F3D3A',
  primaryDeep: '#0A2D2B',
  railHover: '#1B5651',
  railSage: '#9FC4BB',
  railFocus: '#8FE3BE',
  presenceGreen: '#5FD3A0',
  tallyMint: '#CFE5DE',
  counterMint: '#E6F2EE',
  slipPaper: '#FBFAF6',
  ledgerInk: '#14211F',
  carbonCopy: '#4B615C',
  sealGreen: '#1E8A5A',
  draftAmber: '#C98A1B',
  billTan: '#B8A27A',
} as const;

export const font = {
  sans: 'Anuphan, "Noto Sans Thai", system-ui, sans-serif',
  mono: '"Noto Sans Mono", Anuphan, monospace',
} as const;

/** The output frame. */
export const VIDEO = { width: 1920, height: 1080, fps: 30 } as const;
