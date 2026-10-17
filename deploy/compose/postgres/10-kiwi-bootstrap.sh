# Copied from deploy/k8s/overlays/local/deps/postgres-init.yaml: the four K.I.W.I. roles, the database and pgvector.
set -eu
psql -v ON_ERROR_STOP=1 --no-psqlrc --username "$POSTGRES_USER" --dbname postgres <<'SQL'
SET log_min_error_statement = panic;
\set VERBOSITY terse
\set pw `printf %s "$KIWI_DB_PASSWORD"`
CREATE ROLE kiwi_owner LOGIN PASSWORD :'pw';
CREATE ROLE kiwi_app LOGIN PASSWORD :'pw';
CREATE ROLE kiwi_system LOGIN BYPASSRLS PASSWORD :'pw';
CREATE ROLE kiwi_ro LOGIN PASSWORD :'pw';
CREATE DATABASE kiwi OWNER kiwi_owner;
\connect kiwi
CREATE EXTENSION vector;
SQL
