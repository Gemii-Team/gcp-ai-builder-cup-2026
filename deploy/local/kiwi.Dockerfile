# The image Tilt deploys as kiwi (and as the migrate Job): the same layout as
# backend/Dockerfile's final stage, but around a binary that Tilt cross-compiled
# on the host, so a rebuild is one COPY layer. The context is a directory that
# holds only that binary (see the kiwi custom_build in the Tiltfile).
FROM gcr.io/distroless/static:nonroot
COPY kiwi /app/kiwi
ENTRYPOINT ["/app/kiwi"]
