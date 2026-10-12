# One Cloud Run service, one instance, two containers.
#
#   web   the ingress container (Caddy): serves the PWA, proxies /api and /ws to localhost.
#         Listening on the one port Cloud Run routes traffic to.
#   kiwi  the sidecar: the one binary, every role, the in-process bus. It listens on its own port
#         (KIWI_LISTEN_ADDR, all interfaces so the platform's probes reach it); Cloud Run routes
#         outside traffic only to the ingress container's port, so only web can reach it.
#
# Exactly one instance (min = max = 1) because the bus is in-process and the Meet poll, the
# outbox relay and delayed jobs run in the process itself: two instances would each run them
# (architecture review question (a)). CPU is always allocated (instance-based billing) because
# those loops run between requests; Cloud Run requires a whole vCPU per container for that. A
# rollout still briefly runs two instances (the new revision starts before traffic moves).
resource "google_cloud_run_v2_service" "this" {
  project  = var.project_id
  name     = var.service_name
  location = var.region
  labels   = var.labels

  ingress             = "INGRESS_TRAFFIC_ALL"
  launch_stage        = "GA"
  deletion_protection = var.deletion_protection

  template {
    service_account       = var.run_service_account_email
    execution_environment = "EXECUTION_ENVIRONMENT_GEN2" # the PDF reader confines a child process; gen1's sandbox may refuse it (F25)
    timeout               = "${var.request_timeout_seconds}s"

    max_instance_request_concurrency = var.max_instance_request_concurrency

    scaling {
      min_instance_count = 1
      max_instance_count = 1
    }

    # Direct VPC egress (no connector, no Cloud NAT). Only traffic to private ranges goes through
    # the VPC (Cloud SQL, Valkey); Vertex AI, Cloud Storage and Google sign-in leave Cloud Run
    # directly, which the app needs at every start (OIDC discovery, F44).
    vpc_access {
      egress = "PRIVATE_RANGES_ONLY"
      network_interfaces {
        network    = var.network_name
        subnetwork = var.subnet_name
      }
    }

    containers {
      name       = "web"
      image      = local.web_image
      depends_on = ["kiwi"]

      ports {
        container_port = var.web_port
      }

      resources {
        limits = {
          cpu    = var.web_cpu
          memory = var.web_memory
        }
        cpu_idle          = false
        startup_cpu_boost = true
      }

      dynamic "env" {
        for_each = local.web_env
        content {
          name  = env.key
          value = env.value
        }
      }

      # Caddy answers this itself, so the probe says only that the ingress container is up.
      # kiwi's own health is probed on the sidecar, which is what the instance depends on.
      startup_probe {
        http_get {
          path = "/healthz"
          port = var.web_port
        }
        period_seconds    = 2
        timeout_seconds   = 1
        failure_threshold = 30
      }
      liveness_probe {
        http_get {
          path = "/healthz"
          port = var.web_port
        }
        period_seconds    = 10
        timeout_seconds   = 3
        failure_threshold = 6
      }
    }

    containers {
      name  = "kiwi"
      image = local.kiwi_image

      resources {
        limits = {
          cpu    = var.kiwi_cpu
          memory = local.kiwi_memory
        }
        cpu_idle = false
      }

      dynamic "env" {
        for_each = local.kiwi_env
        content {
          name  = env.key
          value = env.value
        }
      }

      # One secret per variable, each read from Secret Manager when an instance starts.
      dynamic "env" {
        for_each = var.runtime_secret_ids
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = env.value
              version = "latest"
            }
          }
        }
      }

      # /readyz pings Postgres and Valkey, so it passes only when both are reachable through the
      # VPC. The budget is three minutes: Direct VPC egress can delay the start by a minute or more.
      startup_probe {
        http_get {
          path = "/readyz"
          port = var.kiwi_port
        }
        period_seconds    = 5
        timeout_seconds   = 3
        failure_threshold = 36
      }
      liveness_probe {
        http_get {
          path = "/healthz"
          port = var.kiwi_port
        }
        period_seconds    = 10
        timeout_seconds   = 3
        failure_threshold = 6
      }
    }
  }

  lifecycle {
    # CI owns the images once the first apply is done (gcloud run deploy / the deploy action):
    # without this every apply would put the bootstrap image back. The labels and the client
    # fields are what those tools stamp on a deployed service.
    ignore_changes = [
      template[0].containers[0].image,
      template[0].containers[1].image,
      template[0].labels,
      labels,
      client,
      client_version,
    ]

    precondition {
      condition     = length(setintersection(keys(var.extra_env), keys(local.core_env))) == 0
      error_message = "extra_env must not redefine a variable this module manages: ${join(", ", sort(tolist(setintersection(keys(var.extra_env), keys(local.core_env)))))}."
    }
  }
}
