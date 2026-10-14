variable "project_id" {
  description = "The project the service and the Jobs run in."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "region" {
  description = "The region of the service and the Jobs; it must be the region of the network and of the data stores."
  type        = string
  default     = "asia-southeast1"
}

# --- names -----------------------------------------------------------------------------------

variable "service_name" {
  description = "The Cloud Run service name. The default URL embeds it, so it must equal the service_name given to the public-url module."
  type        = string
  default     = "kiwi-app"
}

variable "migrate_job_name" {
  description = "Name of the Job that applies the migrations and bootstraps the database roles."
  type        = string
  default     = "kiwi-migrate"
}

variable "seed_job_name" {
  description = "Name of the Job that seeds a workspace."
  type        = string
  default     = "kiwi-seed"
}

# --- identities ------------------------------------------------------------------------------

variable "run_service_account_email" {
  description = "The identity of the service (kiwi-run): it sees no owner credential."
  type        = string
}

variable "jobs_service_account_email" {
  description = "The identity of the Jobs (kiwi-jobs): the only one whose Jobs receive the table owner's connection string."
  type        = string
}

variable "deploy_member" {
  description = "IAM member (serviceAccount:...) that may deploy new revisions of the service and update and run the Jobs, on these resources alone. Empty for none."
  type        = string
  default     = ""
}

variable "allow_public_access" {
  description = "Whether anyone may call the service (allUsers as invoker). The app authenticates its own users, so it is public by decision; the Jobs never are."
  type        = bool
  default     = true
}

# --- network ---------------------------------------------------------------------------------

variable "network_name" {
  description = "The VPC network for Direct VPC egress."
  type        = string
}

variable "subnet_name" {
  description = "The subnet for Direct VPC egress (a /26 or larger)."
  type        = string
}

# --- shape and images ------------------------------------------------------------------------

variable "single_container" {
  description = <<-EOT
    The "minimal" profile's shape: one container, the all-in-one kiwi image, which serves the web
    app itself (KIWI_WEB_DIR) and listens on the port Cloud Run passes ($PORT). There is no Caddy
    ingress container and no sidecar: web_image, web_cpu, web_memory and kiwi_port are not used.
    False builds the "full" profile: a Caddy ingress container plus the kiwi sidecar.
  EOT
  type        = bool
  default     = false
}

variable "web_image" {
  description = <<-EOT
    Full profile only: the ingress (Caddy) image, or null for Google's public hello image. The
    first apply must succeed before CI has built anything, so a placeholder is the default. After
    that CI owns the image: the service ignores later changes of it (see lifecycle), so applying
    never rolls a deployed image back. Must be null when single_container is true: there is no
    web image in that profile, and an ignored value would be a dangling reference.
  EOT
  type        = string
  default     = null

  validation {
    condition     = var.web_image == null || !var.single_container
    error_message = "web_image must be null when single_container is true: the minimal profile has one image, kiwi, which contains the web app."
  }
}

variable "kiwi_image" {
  description = <<-EOT
    The kiwi image, used by the service (the sidecar in the full profile, the only container in
    the minimal one) and by both Jobs, or null for placeholders. A placeholder service container
    is Google's public hello image; a placeholder Job is an image with no program in it, so
    executing a Job before CI has deployed the real image fails at once instead of "succeeding"
    without having migrated anything. As for web_image, CI owns the image after the first apply.
  EOT
  type        = string
  default     = null
}

# --- sizing ----------------------------------------------------------------------------------

variable "web_cpu" {
  description = "vCPU of the ingress container. Cloud Run bills instance-based CPU in whole vCPUs: below 1 it would not be allowed with CPU always allocated."
  type        = string
  default     = "1"
}

variable "web_memory" {
  description = "Memory of the ingress container."
  type        = string
  default     = "512Mi"
}

variable "kiwi_cpu" {
  description = "vCPU of the kiwi container."
  type        = string
  default     = "1"
}

variable "kiwi_memory_gib" {
  description = "Memory of the kiwi container in GiB. At least 2: recordings of up to 200 MiB are held whole in memory, up to four at a time, next to two PDF reader children (architecture review F12, F25). GOMEMLIMIT is set to 80% of it."
  type        = number
  default     = 2

  validation {
    condition     = var.kiwi_memory_gib >= 2 && floor(var.kiwi_memory_gib) == var.kiwi_memory_gib
    error_message = "kiwi_memory_gib must be a whole number of at least 2."
  }
}

variable "max_instance_request_concurrency" {
  description = "Concurrent requests one instance accepts. Every open WebSocket is a request, and there is exactly one instance, so the default of 80 would cap users, not CPU; 1000 is Cloud Run's maximum."
  type        = number
  default     = 1000

  validation {
    condition     = var.max_instance_request_concurrency >= 80 && var.max_instance_request_concurrency <= 1000
    error_message = "max_instance_request_concurrency must be between 80 and 1000."
  }
}

variable "request_timeout_seconds" {
  description = "Request timeout. A WebSocket is one request, so this is also how long a socket lives (3600 is the maximum); clients reconnect and resynchronise."
  type        = number
  default     = 3600

  validation {
    condition     = var.request_timeout_seconds >= 300 && var.request_timeout_seconds <= 3600
    error_message = "request_timeout_seconds must be between 300 and 3600."
  }
}

variable "web_port" {
  description = "The port the ingress container listens on (Cloud Run passes it as PORT): Caddy in the full profile, whose Caddyfile hard-codes 8080, and kiwi itself in the minimal one, which listens on :$PORT when KIWI_LISTEN_ADDR is empty."
  type        = number
  default     = 8080
}

variable "kiwi_port" {
  description = "Full profile only: the port the kiwi sidecar listens on, on the instance's loopback. The two containers share one network namespace, so it must differ from web_port."
  type        = number
  default     = 8081

  validation {
    condition     = var.kiwi_port != var.web_port && var.kiwi_port > 1023 && var.kiwi_port < 65536
    error_message = "kiwi_port must be an unprivileged port different from web_port."
  }
}

variable "migrate_timeout_seconds" {
  description = "How long the migrate Job may run."
  type        = number
  default     = 600
}

variable "seed_timeout_seconds" {
  description = "How long the seed Job may run (it embeds the demo corpus through Vertex AI before it returns)."
  type        = number
  default     = 1800
}

# --- application configuration ---------------------------------------------------------------

variable "public_url" {
  description = "KIWI_PUBLIC_URL: the origin of the app (from the public-url module): https, a host, no path, no trailing slash."
  type        = string

  validation {
    condition     = can(regex("^https://[a-z0-9.-]+$", var.public_url))
    error_message = "public_url must be an https origin with no port, path or trailing slash: the app compares the browser's Origin with it exactly."
  }
}

variable "blob_url" {
  description = "KIWI_BLOB_URL: gs://<bucket> of the uploads bucket."
  type        = string

  validation {
    condition     = can(regex("^gs://[a-z0-9._-]+$", var.blob_url))
    error_message = "blob_url must be gs://<bucket> with no path."
  }
}

variable "valkey_url" {
  description = <<-EOT
    KIWI_VALKEY_URL: redis://<host>:<port> of the cluster-disabled Valkey instance, or null for
    none. Null makes the cache the in-process one (KIWI_CACHE_PROVIDER=memory), which is why a
    service without Valkey is exactly one instance: the cache and the live-frame fan-out are
    per process (see the scaling block in service.tf).
  EOT
  type        = string
  default     = null

  validation {
    condition     = var.valkey_url == null || can(regex("^redis://[^/@]+$", var.valkey_url))
    error_message = "valkey_url must be redis://<host>:<port>, with no credential (authentication is off) and no path."
  }
}

# --- database pools ---------------------------------------------------------------------------

variable "db_app_max_conns" {
  description = "KIWI_DB_APP_MAX_CONNS: the size of the kiwi_app pool of each process (the service and the seed Job), or null to leave the app's own default. Count it in the Cloud SQL max_connections budget: see the cloudsql module."
  type        = number
  default     = null

  validation {
    condition     = var.db_app_max_conns == null || (var.db_app_max_conns >= 1 && floor(var.db_app_max_conns) == var.db_app_max_conns)
    error_message = "db_app_max_conns must be a whole number of at least 1, or null."
  }
}

variable "db_system_max_conns" {
  description = "KIWI_DB_SYSTEM_MAX_CONNS: the size of the kiwi_system pool of each process, or null to leave the app's own default. At least 6: the outbox relay holds 4 system connections while jobs run, and 2 more are headroom. Set both pools or neither."
  type        = number
  default     = null

  validation {
    condition     = var.db_system_max_conns == null || (var.db_system_max_conns >= 6 && floor(var.db_system_max_conns) == var.db_system_max_conns)
    error_message = "db_system_max_conns must be a whole number of at least 6 (the relay holds 4 system connections during jobs, plus 2 of headroom), or null."
  }

  validation {
    condition     = (var.db_app_max_conns == null) == (var.db_system_max_conns == null)
    error_message = "Set both db_app_max_conns and db_system_max_conns, or neither: the Cloud SQL connection budget needs both."
  }
}

variable "db_max_conn_idle" {
  description = "KIWI_DB_MAX_CONN_IDLE: a Go duration (for example 2m) after which an idle database connection is closed, or null to leave the app's own default. Closing idle connections is what keeps the old revision's pools small while a rollout lets it linger."
  type        = string
  default     = null

  validation {
    condition     = var.db_max_conn_idle == null || can(regex("^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$", var.db_max_conn_idle))
    error_message = "db_max_conn_idle must be a Go duration such as 30s, 2m or 1h30m, or null."
  }
}

variable "runtime_secret_ids" {
  description = "Environment variable name to Secret Manager secret id, for what the service and the seed Job read: the app and system connection strings and the application secrets. The latest version is read when an instance starts."
  type        = map(string)
}

variable "owner_secret_ids" {
  description = "Environment variable name to secret id of the table owner's connection string. Only the migrate Job receives it."
  type        = map(string)
}

variable "readonly_secret_ids" {
  description = "Environment variable name to secret id of the read-only role's connection string. Only the migrate Job receives it, for the bootstrap that creates the role."
  type        = map(string)
}

variable "vertex_project_id" {
  description = "The project Vertex AI is called in. Null means project_id."
  type        = string
  default     = null
}

variable "vertex_location" {
  description = <<-EOT
    The Vertex AI location for generation (and the ADK agents). Not decided yet. asia-southeast1
    serves gemini-3.5-flash and keeps processing in Singapore; "global" serves newer models but
    Google chooses where it is processed. Switching is a change of this one value.
  EOT
  type        = string
  default     = "asia-southeast1"
}

variable "vertex_embedding_location" {
  description = "The Vertex AI location for embeddings. gemini-embedding-001 is not served by the global endpoint, so this must be a region."
  type        = string
  default     = "asia-southeast1"

  validation {
    condition     = var.vertex_embedding_location != "global"
    error_message = "gemini-embedding-001 is not available on the global endpoint: vertex_embedding_location must be a region."
  }
}

variable "vertex_model_flash" {
  description = "Model id for extraction, answers and the agents (KIWI_LLM_MODEL_FLASH)."
  type        = string
  default     = "gemini-3.5-flash"
}

variable "vertex_model_pro" {
  description = "Model id of the stronger tier (KIWI_LLM_MODEL_PRO), or null to use the Flash model: no Pro model is served at asia-southeast1."
  type        = string
  default     = null
}

variable "vertex_asr_model" {
  description = "Model id that transcribes recordings (KIWI_ASR_MODEL), or null to use the Flash model."
  type        = string
  default     = null
}

variable "vertex_embedding_model" {
  description = "Model id of the embeddings (KIWI_EMBED_MODEL)."
  type        = string
  default     = "gemini-embedding-001"
}

variable "vertex_embedding_dimensions" {
  description = "Width of the embedding vectors. The schema is vector(768) in the migrations, so this must stay 768 until a migration changes the column."
  type        = number
  default     = 768

  validation {
    condition     = var.vertex_embedding_dimensions == 768
    error_message = "The database column is vector(768); change the migrations before changing the width."
  }
}

variable "extra_env" {
  description = "Further plain (non-secret) environment variables of the kiwi container and the seed Job, for example KIWI_WORKSPACE_NAME."
  type        = map(string)
  default     = {}
}

# --- lifecycle -------------------------------------------------------------------------------

variable "deletion_protection" {
  description = "Whether Terraform refuses to destroy the service and the Jobs."
  type        = bool
  default     = true
}

variable "labels" {
  description = "Labels of the service and the Jobs."
  type        = map(string)
  default     = {}
}
