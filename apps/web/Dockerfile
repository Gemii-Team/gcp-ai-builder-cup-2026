# The web image: the Vite build, served by Caddy on port 8080 as user 65532.
# The build context is the repository root, because the app depends on the workspace packages:
#   docker build -f apps/web/Dockerfile .

FROM node:22-alpine AS build
RUN npm install -g pnpm@10.28.0
WORKDIR /repo

# Fetch from the lockfile alone, so this layer survives source edits.
COPY pnpm-lock.yaml ./
RUN pnpm fetch

COPY package.json pnpm-workspace.yaml ./
COPY apps/web apps/web
COPY packages packages
RUN pnpm install --offline --frozen-lockfile --filter web...
RUN pnpm --filter web build

# The base tag floats (caddy:2-alpine moves with every 2.x release). Nothing here depends on a
# patch version, but it does depend on what the base ships: see the capability below.
FROM caddy:2-alpine
# The base image gives /usr/bin/caddy the file capability cap_net_bind_service=ep so that it can
# bind port 80 as a non-root user. Port 8080 needs none, and a container that drops ALL
# capabilities cannot exec a binary that carries one (the kernel answers EPERM). It is removed
# when there is one, so a later base without it still builds; a getcap or setcap that fails
# fails the build, and chown is not tolerated either.
RUN caps="$(getcap /usr/bin/caddy)" \
 && { [ -z "$caps" ] || setcap -r /usr/bin/caddy; } \
 && chown -R 65532:65532 /data /config
COPY apps/web/Caddyfile /etc/caddy/Caddyfile
COPY --from=build /repo/apps/web/dist /usr/share/caddy
# Caddy writes only to /data, /config and /tmp (the Deployment mounts emptyDirs there and
# keeps the root filesystem read-only).
USER 65532:65532
EXPOSE 8080
