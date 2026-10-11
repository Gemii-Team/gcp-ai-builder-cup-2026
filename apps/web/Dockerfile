# The web image: the Vite build, served by Caddy on port 80.
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

FROM caddy:2-alpine
COPY apps/web/Caddyfile /etc/caddy/Caddyfile
COPY --from=build /repo/apps/web/dist /usr/share/caddy
EXPOSE 80
