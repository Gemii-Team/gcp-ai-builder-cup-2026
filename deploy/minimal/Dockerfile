# syntax=docker/dockerfile:1
# The all-in-one image of the minimal profile: the kiwi binary and the built web app in one
# distroless image. kiwi serves the web app itself (KIWI_WEB_DIR) next to /api, /ws,
# /healthz and /readyz, so nothing else needs to run beside it: no Caddy, no second image.
# `kiwi` with no argument runs `serve`; `kiwi migrate` and `kiwi seed` are subcommands, so the
# migrate Job and `task seed` run this same image.
#
# The build context is the repository root, because the build needs backend/, apps/web and
# packages/ together:
#   docker build -f deploy/minimal/Dockerfile .
# It sits here and not next to the sources because backend/Dockerfile and apps/web/Dockerfile
# each build one thing from their own context, and the full stack keeps using them untouched.
# The context is filtered by Dockerfile.dockerignore (BuildKit reads <name>.dockerignore).
#
# The base tags float, as in backend/Dockerfile and apps/web/Dockerfile: golang:1.26 moves
# with every 1.26 patch release, node:22-alpine with every 22.x, and the distroless tag with
# the image's own releases. Nothing here depends on a patch version, and nothing is pinned
# that the other two files do not pin; only pnpm is, by version, as in apps/web/Dockerfile.

# The web build, as apps/web/Dockerfile builds it.
FROM node:22-alpine AS web
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

# The Go build, as backend/Dockerfile builds it (same base, same flags, same output). The
# module cache and the build cache are BuildKit cache mounts, so a rebuild after a one-file
# change recompiles that package and not the world: Tilt rebuilds this image on every save.
# The embedded migrations (backend/db/migrations) are part of the binary.
FROM golang:1.26 AS gobuild
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY backend/ ./
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/kiwi ./cmd/kiwi

# The runtime image has no shell, so nothing here runs: files are copied and that is all.
# USER stays what the base sets (nonroot, 65532:65532), which the Deployment relies on
# (runAsUser 65532, a read-only root filesystem: kiwi writes only to /data and /tmp).
FROM gcr.io/distroless/static:nonroot
COPY --from=gobuild /out/kiwi /app/kiwi
COPY --from=web /repo/apps/web/dist /app/web
ENV KIWI_WEB_DIR=/app/web
EXPOSE 8080
ENTRYPOINT ["/app/kiwi"]
