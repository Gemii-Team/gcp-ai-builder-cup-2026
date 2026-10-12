// Renders the PWA icons (manifest 192 and 512, the maskable 512, the iOS home-screen icon) from the brand
// app icon. The PNGs are committed: run this after changing apps/web/public/brand/kiwi-app-icon.svg.
//
//   pnpm install && node scripts/web/make-icons.mjs
//
// The app icon is a full-bleed square whose mark stays inside the central 80% safe zone, so one render
// serves both the "any" and the "maskable" purpose.
import { mkdir, readFile } from 'node:fs/promises';
import sharp from 'sharp';

const publicDir = new URL('../../apps/web/public/', import.meta.url);
const outputs = [
  ['icon-192.png', 192],
  ['icon-512.png', 512],
  ['icon-maskable-512.png', 512],
  ['apple-touch-icon.png', 180],
];

const svg = await readFile(new URL('brand/kiwi-app-icon.svg', publicDir));
await mkdir(new URL('icons/', publicDir), { recursive: true });

for (const [name, size] of outputs) {
  // Rasterise at 4x the artwork size, then scale down, so the dashed seal ring stays crisp.
  await sharp(svg, { density: 288 })
    .resize(size, size)
    .png({ compressionLevel: 9 })
    .toFile(new URL(`icons/${name}`, publicDir).pathname);
  console.log(`icons/${name} ${size}x${size}`);
}
