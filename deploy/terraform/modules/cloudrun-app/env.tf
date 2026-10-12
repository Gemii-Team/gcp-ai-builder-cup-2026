# The environment of the binary on Cloud Run, mapped from the variables it reads
# (backend/internal/platform/config/config.go) plus the ones the architecture review names for
# the parts that do not exist yet. "read today" is checked against config.go; the rest are
# app-side dependencies this module can set but cannot make the binary honour. Nothing is
# faked to get around one (no dummy value for a variable that would silence a check).
#
#   variable                    value                                  status in the binary
#   --------------------------  -------------------------------------  ------------------------------
#   KIWI_ENV                    prod                                   read; F06: should also be required when K_SERVICE is set
#   KIWI_ROLES                  api,realtime,worker,agents             read (one process runs every role)
#   KIWI_PUBLIC_URL             var.public_url                         read
#   KIWI_BUS_PROVIDER           memory                                 read (the only bus; one instance only)
#   KIWI_VALKEY_URL             var.valkey_url                         read
#   KIWI_BLOB_URL               var.blob_url (gs://...)                read, but config.go refuses every scheme but file:// (F03: GCS adapter)
#   KIWI_LLM_PROVIDER           vertex                                 read; no vertex adapter is built (F01, F05)
#   KIWI_LLM_MODEL_FLASH / _PRO var.vertex_model_flash / _pro          read
#   KIWI_ASR_MODEL              var.vertex_asr_model                   read
#   KIWI_EMBED_MODEL            var.vertex_embedding_model             read
#   KIWI_LISTEN_ADDR            :<kiwi_port>                           NOT read: both containers hard-code :8080 (F07)
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
# Not set, on purpose: KIWI_OIDC_ISSUER and KIWI_OIDC_INTERNAL_URL (the default is Google's, and a
# prod process refuses anything else). KIWI_LLM_BASE_URL, KIWI_LLM_API_KEY and KIWI_EMBED_BASE_URL
# are the LiteLLM and Ollama endpoints; config.Load still requires them of the worker and agents
# roles, which makes a Vertex-only deployment impossible until they become optional for the
# vertex provider (F01). The ingress container gets KIWI_UPSTREAM and KIWI_PUBLIC_URL for the
# production Caddyfile (proxy /api and /ws to the sidecar, redirect other hosts: F07, F17).
locals {
  vertex_project = coalesce(var.vertex_project_id, var.project_id)
  model_pro      = coalesce(var.vertex_model_pro, var.vertex_model_flash)
  asr_model      = coalesce(var.vertex_asr_model, var.vertex_model_flash)

  kiwi_memory    = "${var.kiwi_memory_gib}Gi"
  gomemlimit_mib = floor(var.kiwi_memory_gib * 1024 * 0.8)

  core_env = merge(
    {
      KIWI_ENV                   = "prod"
      KIWI_ROLES                 = "api,realtime,worker,agents"
      KIWI_PUBLIC_URL            = var.public_url
      KIWI_BUS_PROVIDER          = "memory"
      KIWI_VALKEY_URL            = var.valkey_url
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
    var.vertex_location == "global" ? { KIWI_VERTEX_ALLOW_GLOBAL = "true" } : {},
  )

  # The sidecar: everything above, the port it must listen on, and the memory ceiling the Go
  # runtime should respect so the garbage collector works before the container is killed.
  # PORT is for images that read it: the placeholder image listens on it, and a sidecar gets no
  # PORT from Cloud Run (only the ingress container does), so without it the placeholder would
  # try to take the ingress port. The real image ignores it.
  kiwi_env = merge(var.extra_env, local.core_env, {
    KIWI_LISTEN_ADDR = ":${var.kiwi_port}"
    GOMEMLIMIT       = "${local.gomemlimit_mib}MiB"
    PORT             = tostring(var.kiwi_port)
  })

  web_env = {
    KIWI_UPSTREAM   = "localhost:${var.kiwi_port}"
    KIWI_PUBLIC_URL = var.public_url
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
