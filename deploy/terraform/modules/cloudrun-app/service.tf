# One Cloud Run service, one instance. Two shapes (var.single_container):
#
#   minimal (single_container = true) ONE container, `kiwi`: the all-in-one image holds the binary
#         and the built web app (KIWI_WEB_DIR), so kiwi serves the web app, /api and /ws itself on
#         the port Cloud Run passes ($PORT, which it sets because of the `ports` block below).
#         The cache is the in-process one (valkey_url null), so there is no Valkey either.
#   full  (single_container = false)  TWO containers:
#     web   the ingress container (Caddy): serves the PWA, proxies /api and /ws to localhost.
#           Listening on the one port Cloud Run routes traffic to.
#     kiwi  the sidecar: the one binary, every role, the in-process bus. It listens on its own
#           port (KIWI_LISTEN_ADDR, all interfaces so the platform's probes reach it); Cloud Run
#           routes outside traffic only to the ingress container's port, so only web can reach it.
#
# Exactly one instance (min = max = 1) in both shapes because the bus is in-process and the Meet
# poll, the outbox relay and delayed jobs run in the process itself: two instances would each run
# them (architecture review question (a)). In the minimal profile the cache and the live-frame
# fan-out are in-process too (KIWI_CACHE_PROVIDER=memory needs all four roles in ONE process), so
# max_instance_count = 1 is what makes it correct, not only cheap. CPU is always allocated
# (instance-based billing) because those loops run between requests; Cloud Run requires a whole
# vCPU per container for that.
#
# Rollout overlap. A rollout still briefly runs two instances: the new revision's instance starts
# before the traffic moves, and max_instance_count is a limit per revision, so the service can
# exceed it while two revisions are alive
# (https://docs.cloud.google.com/run/docs/about-instance-autoscaling, "Traffic splits").
# Traffic moves once the new instance passes its startup probe; requests already in flight, a
# WebSocket being one, are not dropped and are served to the end by the old revision
# (https://docs.cloud.google.com/run/docs/rollouts-rollbacks-traffic-migration). So the old instance
# lives until its sockets close: when clients reconnect, or when the request timeout (3600 s) ends
# them. With the in-memory cache that has three honest consequences, all bounded by that window:
#   - live frames one revision publishes do not reach the clients connected to the other until
#     they reconnect and resynchronise from Postgres (the app resyncs by sequence number);
#   - a sign-in that started on the old revision keeps its state in that process's memory, so its
#     callback can land on the new revision and fail: the person signs in again;
#   - sessions survive: the app persists them in Postgres, not in the process.
# What is set to keep the overlap short (the new instance's startup is the part a deploy
# controls; the old one's drain is not):
#   - startup_cpu_boost = true: extra CPU while the instance starts and for 10 s after, so the
#     new revision is ready sooner (https://docs.cloud.google.com/run/docs/configuring/services/cpu,
#     "Set startup CPU boost"); it is billed only for that time;
#   - the startup probe polls /readyz every 2 s (env.tf), so readiness is seen at once;
#   - run deployments outside demo hours: no setting removes the overlap.
# What is not done, and why. The request timeout stays at 3600 s: a shorter one would cut every
# socket at that age to shorten a drain that happens once per deploy. Service-level manual scaling
# (https://docs.cloud.google.com/run/docs/configuring/services/manual-scaling) splits instances
# across revisions only while traffic is split, and a revision that only has a traffic tag starts
# an instance beside the counted ones, so it would not stop the new instance from starting next
# to the old one [INFERENCE from that page; not tried]. The old instance's own shutdown is
# SIGTERM, then SIGKILL 10 s later (https://docs.cloud.google.com/run/docs/container-contract):
# the app must finish in under 8 s (F08), which is app-side work.
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
    # the VPC (Cloud SQL, and Valkey in the full profile); Vertex AI, Cloud Storage and Google
    # sign-in leave Cloud Run directly, which the app needs at every start (OIDC discovery, F44).
    vpc_access {
      egress = "PRIVATE_RANGES_ONLY"
      network_interfaces {
        network    = var.network_name
        subnetwork = var.subnet_name
      }
    }

    # Full profile only. It is declared before the kiwi container so the order is web, kiwi as it
    # was before the minimal profile existed; in the minimal one kiwi is containers[0].
    dynamic "containers" {
      for_each = local.single ? [] : [1]

      content {
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
    }

    containers {
      name  = "kiwi"
      image = local.kiwi_image

      # No `args`: the image's entrypoint is /app/kiwi and the binary runs `serve` when it is
      # given no subcommand (backend/cmd/kiwi/main.go), as in the Kubernetes manifests. The Jobs
      # name their subcommand (jobs.tf). Leaving it unset also lets the placeholder image start.

      # Minimal profile only: kiwi is the ingress container. Cloud Run sets PORT to this value and
      # kiwi listens on :$PORT when KIWI_LISTEN_ADDR is empty.
      dynamic "ports" {
        for_each = local.single ? [1] : []

        content {
          container_port = var.web_port
        }
      }

      resources {
        limits = {
          cpu    = var.kiwi_cpu
          memory = local.kiwi_memory
        }
        cpu_idle = false
        # The minimal profile's kiwi is the whole instance, so a faster start is a shorter
        # rollout overlap (see above). The full profile's sidecar keeps its previous setting.
        startup_cpu_boost = local.single ? true : null
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

      # /readyz pings Postgres (and Valkey in the full profile), so it passes only when they are
      # reachable through the VPC. The budget is three minutes: Direct VPC egress can delay the
      # start by a minute or more. The probes hit kiwi itself, not an ingress in front of it.
      startup_probe {
        http_get {
          path = "/readyz"
          port = local.kiwi_probe_port
        }
        period_seconds    = local.kiwi_startup_probe.period_seconds
        timeout_seconds   = local.kiwi_startup_probe.timeout_seconds
        failure_threshold = local.kiwi_startup_probe.failure_threshold
      }
      liveness_probe {
        http_get {
          path = "/healthz"
          port = local.kiwi_probe_port
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
    # fields are what those tools stamp on a deployed service. ignore_changes cannot depend on a
    # variable, so both container indexes are listed: the full profile has web at 0 and kiwi at 1,
    # the minimal one has kiwi at 0 and nothing at 1 (a path that is not there ignores nothing).
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
