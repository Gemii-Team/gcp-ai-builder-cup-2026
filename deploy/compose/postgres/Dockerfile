# pgvector with the K.I.W.I. role bootstrap baked in (deploy/k8s/overlays/local/deps/postgres-init.yaml), so nothing is
# bind-mounted at run time.
FROM pgvector/pgvector:0.8.7-pg17
COPY --chmod=0755 10-kiwi-bootstrap.sh /docker-entrypoint-initdb.d/10-kiwi-bootstrap.sh
