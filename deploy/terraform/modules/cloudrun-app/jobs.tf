# The two Jobs, the same image as the service (in the minimal profile that is the one all-in-one
# kiwi image; there is no other):
#
#   migrate  `kiwi migrate`: applies the migrations as the table owner and (app-side, F02)
#            bootstraps the kiwi_app, kiwi_system and kiwi_ro roles from the passwords in their
#            connection strings. The only workload that is handed the owner's string.
#   seed     `kiwi seed`: seeds a workspace. Run it with the members as arguments:
#            gcloud run jobs execute <seed job> --args=seed,--members,<emails>
#            (the addresses are not in this repository; F20 asks for the canonical list).
#
# One task, no retries, no parallelism: a migration that fails must be read, not run again by
# itself, and two at once would race (F27). Both run as kiwi-jobs and reach Cloud SQL (and Valkey
# in the full profile) through the same Direct VPC egress as the service.
#
# The seed Job loads the configuration of `kiwi serve`, so it gets the same cache and pool
# variables as the service (env.tf) and opens its own pools: its app and system pools are part of
# the Cloud SQL max_connections budget (modules/cloudsql/main.tf).
resource "google_cloud_run_v2_job" "migrate" {
  project  = var.project_id
  name     = var.migrate_job_name
  location = var.region
  labels   = var.labels

  launch_stage        = "GA"
  deletion_protection = var.deletion_protection

  template {
    parallelism = 1
    task_count  = 1

    template {
      service_account       = var.jobs_service_account_email
      execution_environment = "EXECUTION_ENVIRONMENT_GEN2"
      max_retries           = 0
      timeout               = "${var.migrate_timeout_seconds}s"

      vpc_access {
        egress = "PRIVATE_RANGES_ONLY"
        network_interfaces {
          network    = var.network_name
          subnetwork = var.subnet_name
        }
      }

      containers {
        name  = "migrate"
        image = local.job_image
        args  = ["migrate"]

        resources {
          limits = {
            cpu    = "1"
            memory = "512Mi"
          }
        }

        dynamic "env" {
          for_each = local.migrate_env
          content {
            name  = env.key
            value = env.value
          }
        }

        dynamic "env" {
          for_each = local.migrate_secret_ids
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
      }
    }
  }

  lifecycle {
    ignore_changes = [
      template[0].template[0].containers[0].image,
      template[0].labels,
      labels,
      client,
      client_version,
    ]
  }
}

resource "google_cloud_run_v2_job" "seed" {
  project  = var.project_id
  name     = var.seed_job_name
  location = var.region
  labels   = var.labels

  launch_stage        = "GA"
  deletion_protection = var.deletion_protection

  template {
    parallelism = 1
    task_count  = 1

    template {
      service_account       = var.jobs_service_account_email
      execution_environment = "EXECUTION_ENVIRONMENT_GEN2"
      max_retries           = 0
      timeout               = "${var.seed_timeout_seconds}s"

      vpc_access {
        egress = "PRIVATE_RANGES_ONLY"
        network_interfaces {
          network    = var.network_name
          subnetwork = var.subnet_name
        }
      }

      containers {
        name  = "seed"
        image = local.job_image
        args  = ["seed"]

        resources {
          limits = {
            cpu    = "1"
            memory = "1Gi"
          }
        }

        dynamic "env" {
          for_each = local.seed_env
          content {
            name  = env.key
            value = env.value
          }
        }

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
      }
    }
  }

  lifecycle {
    ignore_changes = [
      template[0].template[0].containers[0].image,
      template[0].labels,
      labels,
      client,
      client_version,
    ]
  }
}
