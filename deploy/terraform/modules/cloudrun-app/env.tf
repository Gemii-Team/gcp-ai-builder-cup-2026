# The environment of the binary on Cloud Run, mapped from the variables it reads
# (backend/internal/platform/config/config.go) plus the ones the architecture review names for
# the parts that do not exist yet. "read" is checked against config.go as merged with the
# minimal-profile app work; "NOT read" is an app-side dependency this module can set but cannot
# make the binary honour. Nothing is faked to get around one (no dummy value for a variable that
# would silence a check).
#
#   variable                    value                                  status in the binary
#   --------------------------  -------------------------------------  ------------------------------
#   KIWI_ENV                    prod                                   read; F06: should also be required when K_SERVICE is set
#   KIWI_ROLES                  api,realtime,worker,agents             read (one process runs every role)
#   KIWI_PUBLIC_URL             var.public_url                         read
#   KIWI_BUS_PROVIDER           memory                                 read (the only bus; one instance only)
#   KIWI_VALKEY_URL             var.valkey_url (full profile only)     read; required only when KIWI_CACHE_PROVIDER is valkey (the default)
#   KIWI_CACHE_PROVIDER         memory (minimal profile only)          read: the cache, tickets, OAuth state and live-frame fan-out are
#                                                                      in-process and sessions are rows in Postgres, so all four roles
#                                                                      must run in ONE process: the service is exactly one instance
#   KIWI_WEB_DIR                /app/web (minimal profile only)        read: kiwi serves the built web app itself (the image also sets it)
#   KIWI_DB_APP_MAX_CONNS       var.db_app_max_conns                   read (F10: done); set only when given, the app's own default is 20
#   KIWI_DB_SYSTEM_MAX_CONNS    var.db_system_max_conns                read; at least 6 (the relay holds 4 system connections, plus 2); default 16
#   KIWI_DB_MAX_CONN_IDLE       var.db_max_conn_idle                   read; a Go duration, idle connections are closed after it; default 30m
#   KIWI_LISTEN_ADDR            :<kiwi_port> (full profile only)       read; empty means :$PORT, and without PORT :8080 (F07: done). The
#                                                                      minimal profile leaves it EMPTY so kiwi listens on the port Cloud Run
#                                                                      gives the ingress container; the full profile's sidecar needs its own
#   PORT                        <kiwi_port> (full profile sidecar)     read by the binary only when KIWI_LISTEN_ADDR is empty. Cloud Run
#                                                                      injects PORT into the ingress container and documents it as reserved
#                                                                      ("you shouldn't set it yourself"); the minimal profile never sets it
#   KIWI_BLOB_URL               var.blob_url (gs://...)                read, but config.go refuses every scheme but file:// (F03: GCS adapter)
#   KIWI_LLM_PROVIDER           vertex                                 read; no vertex adapter is built (F01, F05)
#   KIWI_LLM_MODEL_FLASH / _PRO var.vertex_model_flash / _pro          read
#   KIWI_ASR_MODEL              var.vertex_asr_model                   read
#   KIWI_EMBED_MODEL            var.vertex_embedding_model             read
#   KIWI_VERTEX_PROJECT         var.vertex_project_id                  NOT read (F45)
#   KIWI_VERTEX_LOCATION        var.vertex_location                    NOT read (F45)
#   KIWI_VERTEX_EMBED_LOCATION  var.vertex_embedding_location          NOT read (F45)
#   KIWI_VERTEX_ALLOW_GLOBAL    true, only when the location is global NOT read (F45): residency is not lost silently
#   KIWI_EMBED_DIMENSIONS       768                                    NOT read: the width is fixed in three places (F05)
#   GOMEMLIMIT                  80% of the kiwi memory                 read by the Go runtime (F12)
#   secrets (var.runtime_secret_ids): KIWI_DATABASE_URL, KIWI_DATABASE_SYSTEM_URL, KIWI_OAUTH_CLIENT_ID,
#     KIWI_OAUTH_CLIENT_SECRET, KIWI_TOKEN_ENC_KEY, KIWI_BLOB_SIGNING_KEY   read
#   The Jobs: KIWI_MIGRATE_DATABASE_URL (migrate only, read), and the app, system and read-only
#     strings for the migrate Job's role bootstrap (NOT built: F02).
#
# Vertex AI in the minimal profile (the project owner's decision of 2026-10-14; live/root.hcl has
# the sources). Generation is on the GLOBAL endpoint: KIWI_VERTEX_LOCATION=global with
# KIWI_LLM_MODEL_FLASH=gemini-3.8-flash and KIWI_LLM_MODEL_PRO=gemini-3.1-pro-preview, and this
# module then also sets KIWI_VERTEX_ALLOW_GLOBAL=true. Google chooses where a global request is
# processed, so GENERATION LEAVES SINGAPORE. Embeddings do not: KIWI_VERTEX_EMBED_LOCATION stays
# asia-southeast1 with gemini-embedding-001, which the global endpoint does not serve (and the
# module refuses a global embedding location); Google commits to no in-region processing for
# them either. All of it is configuration only: none of these variables is read until the Vertex
# adapter exists (F01, F05, F45). Every agent runs on the Flash tier today, so the Pro id is set
# but not called.
#
# The image. The service and both Jobs run ONE image in the minimal profile, built from
# deploy/minimal/Dockerfile with the repository root as the build context
# (`docker build -f deploy/minimal/Dockerfile .`): the binary at /app/kiwi, the web app at
# /app/web, ENV KIWI_WEB_DIR=/app/web, ENTRYPOINT ["/app/kiwi"] and no CMD. This module therefore
# sets no `command` and no `args` on the service (no arguments means `serve`) and the Jobs pass
# only the subcommand (`migrate`, `seed`). The full profile's sidecar runs backend/Dockerfile's
# image (the same binary path and entrypoint, no web app), with a Caddy image in front.
#
# Not set, on purpose: KIWI_OIDC_ISSUER and KIWI_OIDC_INTERNAL_URL (the default is Google's, and a
# prod process refuses anything else). KIWI_LLM_BASE_URL, KIWI_LLM_API_KEY and KIWI_EMBED_BASE_URL
# are the LiteLLM and Ollama endpoints; config.Load still requires them of the worker and agents
# roles, which makes a Vertex-only deployment impossible until they become optional for the
# vertex provider (F01). In the full profile the ingress container gets KIWI_UPSTREAM and
# KIWI_PUBLIC_URL for the production Caddyfile (proxy /api and /ws to the sidecar, redirect other
# hosts: F07, F17); the minimal profile has no such container, and the binary has no host
# redirect of its own (F17).
locals {
  # The minimal profile: one container, kiwi, which is also the ingress container.
  single = var.single_container

  vertex_project = coalesce(var.vertex_project_id, var.project_id)
  model_pro      = coalesce(var.vertex_model_pro, var.vertex_model_flash)
  asr_model      = coalesce(var.vertex_asr_model, var.vertex_model_flash)

  kiwi_memory    = "${var.kiwi_memory_gib}Gi"
  gomemlimit_mib = floor(var.kiwi_memory_gib * 1024 * 0.8)

  # The all-in-one image keeps the built web app here (deploy/minimal/Dockerfile).
  web_dir = "/app/web"

  # Valkey when there is a URL for one; otherwise the process keeps the cache itself, which only
  # works while every role runs in one process: that is the one-instance rule of service.tf.
  cache_env = var.valkey_url == null ? { KIWI_CACHE_PROVIDER = "memory" } : { KIWI_VALKEY_URL = var.valkey_url }

  # The database pools, set only when given. The service and the seed Job both receive them, and
  # the Cloud SQL max_connections budget counts both (modules/cloudsql/main.tf).
  pool_env = merge(
    var.db_app_max_conns == null ? {} : { KIWI_DB_APP_MAX_CONNS = tostring(var.db_app_max_conns) },
    var.db_system_max_conns == null ? {} : { KIWI_DB_SYSTEM_MAX_CONNS = tostring(var.db_system_max_conns) },
    var.db_max_conn_idle == null ? {} : { KIWI_DB_MAX_CONN_IDLE = var.db_max_conn_idle },
  )

  core_env = merge(
    {
      KIWI_ENV                   = "prod"
      KIWI_ROLES                 = "api,realtime,worker,agents"
      KIWI_PUBLIC_URL            = var.public_url
      KIWI_BUS_PROVIDER          = "memory"
      KIWI_BLOB_URL              = var.blob_url
      KIWI_LLM_PROVIDER          = "vertex"
      KIWI_LLM_MODEL_FLASH       = var.vertex_model_flash
      KIWI_LLM_MODEL_PRO         = local.model_pro
      KIWI_ASR_MODEL             = local.asr_model
      KIWI_EMBED_MODEL           = var.vertex_embedding_model
      KIWI_EMBED_DIMENSIONS      = tostring(var.vertex_embedding_dimensions)
      KIWI_VERTEX_PROJECT        = local.vertex_project
      KIWI_VERTEX_LOCATION       = var.vertex_location
      KIWI_VERTEX_EMBED_LOCATION = var.vertex_embedding_location
    },
    local.cache_env,
    local.pool_env,
    var.vertex_location == "global" ? { KIWI_VERTEX_ALLOW_GLOBAL = "true" } : {},
  )

  # What only the serving container is told, on top of core_env.
  #
  # Full: the sidecar's own port as KIWI_LISTEN_ADDR, and PORT for the placeholder image. The
  # binary no longer needs PORT (it reads KIWI_LISTEN_ADDR first); the hello placeholder listens
  # on $PORT, and a sidecar gets no PORT from Cloud Run (only the ingress container does), so
  # without it the placeholder would take the ingress port. Cloud Run documents PORT as reserved
  # ("you shouldn't set it yourself": docs.cloud.google.com/run/docs/configuring/services/environment-variables);
  # whether it is refused on a sidecar is unverified, and it is how the full profile has always
  # been built. If it is refused, drop PORT here: the real image does not need it.
  #
  # Minimal: the web app's directory, and NO listen address and NO PORT. kiwi listens on :$PORT
  # when KIWI_LISTEN_ADDR is empty, and Cloud Run injects PORT into the ingress container (the
  # container's `ports` block): setting either here would be a second source of truth, and PORT is
  # reserved. KIWI_WEB_DIR is also baked into the image; it is set here so the one place that
  # lists the service's environment shows it.
  serving_env = merge(
    local.single ? { KIWI_WEB_DIR = local.web_dir } : {},
    local.single ? {} : { KIWI_LISTEN_ADDR = ":${var.kiwi_port}", PORT = tostring(var.kiwi_port) },
  )

  # The memory ceiling the Go runtime should respect so the garbage collector works before the
  # container is killed.
  kiwi_env = merge(var.extra_env, local.core_env, local.serving_env, {
    GOMEMLIMIT = "${local.gomemlimit_mib}MiB"
  })

  web_env = {
    KIWI_UPSTREAM   = "localhost:${var.kiwi_port}"
    KIWI_PUBLIC_URL = var.public_url
  }

  # The port the probes of the kiwi container call: its own, which is the ingress port when it is
  # the only container.
  kiwi_probe_port = local.single ? var.web_port : var.kiwi_port

  # The startup probe of the kiwi container. Both budgets are three minutes: Direct VPC egress can
  # delay the start by a minute or more. The minimal profile polls every 2 s instead of 5 s so a
  # new revision is declared ready, and takes the traffic, up to 3 s sooner: during a rollout the
  # old revision and its in-process cache are still alive until then (see service.tf). The probe
  # timeout cannot exceed the period (Cloud Run health checks documentation).
  kiwi_startup_probe = local.single ? {
    period_seconds    = 2
    timeout_seconds   = 2
    failure_threshold = 90
    } : {
    period_seconds    = 5
    timeout_seconds   = 3
    failure_threshold = 36
  }

  # The seed Job runs `kiwi seed`, which loads the configuration of `kiwi serve` (every role).
  seed_env = merge(var.extra_env, local.core_env)

  # The migrate Job needs only the owner's string, plus the other roles' strings for the bootstrap
  # that creates them.
  migrate_env = { KIWI_ENV = "prod" }
  migrate_secret_ids = merge(
    var.owner_secret_ids,
    var.readonly_secret_ids,
    { for name, id in var.runtime_secret_ids : name => id if contains(["KIWI_DATABASE_URL", "KIWI_DATABASE_SYSTEM_URL"], name) },
  )

  # Placeholders that make the first apply succeed before CI has pushed an image. A service
  # revision must start, so its placeholder is an image that serves HTTP on $PORT. A Job must
  # exit, so its placeholder has no program at all: executing it before the real image is
  # deployed fails immediately instead of succeeding without migrating anything.
  placeholder_service_image = "us-docker.pkg.dev/cloudrun/container/hello"
  placeholder_job_image     = "gcr.io/distroless/static:nonroot"

  web_image  = coalesce(var.web_image, local.placeholder_service_image)
  kiwi_image = coalesce(var.kiwi_image, local.placeholder_service_image)
  job_image  = coalesce(var.kiwi_image, local.placeholder_job_image)
}
