# syntax=docker/dockerfile:1
# Build from this directory: docker build backend
# The last stage is the default target: the kiwi image, one binary at /app/kiwi.
# `--target tools` builds the smoke-only servers under tools/ instead (added with
# the smoke harness; until then that target has nothing to build).

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/kiwi ./cmd/kiwi

FROM build AS tools-build
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./tools/...

FROM gcr.io/distroless/static:nonroot AS tools
COPY --from=tools-build /out/ /app/

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kiwi /app/kiwi
ENTRYPOINT ["/app/kiwi"]
