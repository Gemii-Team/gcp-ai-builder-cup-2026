locals {
  zone = coalesce(var.zone, "${var.region}-a")

  # The database roles of the app (backend/db/migrations/0010_rls_and_grants.sql). Terraform
  # creates only the table owner as a Cloud SQL user. Cloud SQL cannot grant BYPASSRLS, and a user
  # created through the API joins cloudsqlsuperuser, which would hand the serving roles far more
  # than RLS allows. kiwi_app, kiwi_system and kiwi_ro are therefore created by a bootstrap step
  # of `kiwi migrate` (architecture review F02); this module only generates their passwords and
  # publishes them as connection strings for that step and for the service.
  roles = {
    owner    = "kiwi_owner"
    app      = "kiwi_app"
    system   = "kiwi_system"
    readonly = "kiwi_ro"
  }

  # One secret per environment variable of the binary (and one for the bootstrap), named after it.
  # Who may read which: the owner and read-only strings only the bootstrap Job; the app and system
  # strings the Cloud Run service and the Jobs. The keys are quoted so a secret-scanning rule that
  # reads a bare KIWI_..._URL = value line as a leaked connection string does not mistake a name for one.
  dsn_secrets = {
    "KIWI_MIGRATE_DATABASE_URL" = { role = "owner", kind = "owner", accessors = var.bootstrap_members }
    "KIWI_DATABASE_URL"         = { role = "app", kind = "runtime", accessors = var.runtime_members }
    "KIWI_DATABASE_SYSTEM_URL"  = { role = "system", kind = "runtime", accessors = var.runtime_members }
    "KIWI_DATABASE_RO_URL"      = { role = "readonly", kind = "readonly", accessors = var.bootstrap_members }
  }
}

resource "google_sql_database_instance" "this" {
  project             = var.project_id
  name                = var.instance_name
  region              = var.region
  database_version    = var.database_version
  deletion_protection = var.deletion_protection

  settings {
    tier    = var.tier
    edition = var.edition

    # Zonal, not high availability: one instance serves the demo and HA doubles the price. A
    # zone outage means restoring from backup (point-in-time recovery below keeps it to minutes of loss).
    availability_type = "ZONAL"
    activation_policy = "ALWAYS"

    disk_type             = "PD_SSD"
    disk_size             = var.disk_size_gb
    disk_autoresize       = true
    disk_autoresize_limit = var.disk_autoresize_limit_gb

    # Guards against deletion from every surface (API, gcloud, console), not only Terraform.
    deletion_protection_enabled = var.deletion_protection

    user_labels = var.labels

    location_preference {
      zone = local.zone
    }

    # The check looks for the legacy require_ssl, which the provider no longer has: ssl_mode =
    # ENCRYPTED_ONLY below is its replacement and rejects every unencrypted connection.
    #trivy:ignore:AVD-GCP-0015
    ip_configuration {
      # Private IP only: the instance has no public address at all. Cloud Run reaches it through
      # Direct VPC egress.
      ipv4_enabled       = false
      private_network    = var.network_id
      allocated_ip_range = var.private_services_range_name
      # TLS is required of every client. The app's DSNs say sslmode=require.
      ssl_mode = "ENCRYPTED_ONLY"
    }

    backup_configuration {
      enabled                        = true
      start_time                     = var.backup_start_time
      point_in_time_recovery_enabled = var.point_in_time_recovery
      transaction_log_retention_days = var.point_in_time_recovery ? 3 : null

      backup_retention_settings {
        retained_backups = var.backup_retained_count
        retention_unit   = "COUNT"
      }
    }

    # Maintenance restarts the instance: Sunday 18:00 UTC is Monday 01:00 in Thailand.
    maintenance_window {
      day          = 7
      hour         = 18
      update_track = "stable"
    }

    # max_connections on db-g1-small
    #
    # The app opens up to 20 (kiwi_app) + 16 (kiwi_system) connections per process, and both sizes
    # are constants in backend/internal/platform/db/db.go, not configuration. Cloud SQL's default
    # max_connections follows memory: 25 on db-f1-micro, 50 on db-g1-small, 100 from 3.75 GB.
    # One instance needs 36 of them. A rollout keeps the old revision alive for as long as its
    # WebSockets stay open (up to the 60-minute request timeout), so two instances need 72, and the
    # migrate and seed Jobs, Cloud SQL's own agents and the reserved superuser slots come on top.
    #
    # The flag may be raised on db-g1-small: the Cloud SQL flags page gives its range as 14 to
    # "varies", bounded only by max_connections + autovacuum_max_workers + max_worker_processes
    # <= 262142, with no cap per tier. Changing it restarts the instance. 100 is the default of
    # the 3.75 GB tier: room for 72 plus the Jobs and the reserved slots with about 15 to spare.
    #
    # Memory risk. db-g1-small has 1.7 GB and shared_buffers defaults to a third of it (about
    # 0.55 GB). Every backend adds private memory: a few MB idle, more while it sorts or scans an
    # HNSW index. About 100 idle backends fit; 100 busy ones doing large sorts at once may not, and
    # an out-of-memory kill restarts the instance. In steady state one revision serves traffic and
    # the other's connections are idle, so the number of busy backends stays near the active
    # pools. [INFERENCE: the per-backend figures are typical PostgreSQL behaviour, not measured
    # here; this is the first thing to check under the k6 run.] If memory pressure shows, raise
    # the tier (db-custom-1-3840) instead of lowering this number below 72 + Jobs.
    #
    # App-side follow-up (architecture review F10): make the pool sizes configuration
    # (KIWI_DB_APP_MAX_CONNS, KIWI_DB_SYSTEM_MAX_CONNS) and close idle connections after about two
    # minutes. With 10 + 6 connections the default of 50 is enough through a rollout.
    dynamic "database_flags" {
      for_each = var.max_connections == null ? [] : [var.max_connections]
      content {
        name  = "max_connections"
        value = tostring(database_flags.value)
      }
    }

    # Connection, checkpoint, lock-wait and temp-file logging: close to free at this volume (the
    # Logging free tier covers it) and what a connection-limit or lock problem needs to be
    # diagnosed. Statement and duration logging stay off: they can write passwords and every query.
    database_flags {
      name  = "log_checkpoints"
      value = "on"
    }
    database_flags {
      name  = "log_connections"
      value = "on"
    }
    database_flags {
      name  = "log_disconnections"
      value = "on"
    }
    database_flags {
      name  = "log_lock_waits"
      value = "on"
    }
    database_flags {
      name  = "log_temp_files"
      value = "0"
    }
  }

  lifecycle {
    # Disk autoresize grows disk_size behind Terraform's back; without this every plan would
    # propose shrinking it, which Cloud SQL answers by replacing the instance.
    ignore_changes = [settings[0].disk_size]

    precondition {
      condition     = var.edition == "ENTERPRISE" || !can(regex("^db-(f1-micro|g1-small|custom-)", var.tier))
      error_message = "Shared-core and db-custom tiers exist only in the ENTERPRISE edition."
    }
  }
}

resource "google_sql_database" "app" {
  project  = var.project_id
  instance = google_sql_database_instance.this.name
  name     = var.database_name
}

# Generated passwords. Ephemeral: they exist while Terraform runs, are handed to write-only
# arguments below, and are in neither the plan nor the state. Alphanumeric only, so a password
# can sit in a connection string without escaping (32 characters, about 190 bits).
ephemeral "random_password" "role" {
  for_each = local.roles

  length  = 32
  special = false
}

# The table owner, the only database user Terraform creates. It owns every table and runs the
# migrations, so it must never be on the serving path: only the Job that migrates sees its string.
resource "google_sql_user" "owner" {
  project  = var.project_id
  instance = google_sql_database_instance.this.name
  name     = local.roles.owner
  type     = "BUILT_IN"

  password_wo         = ephemeral.random_password.role["owner"].result
  password_wo_version = var.credentials_version

  # A Postgres user that owns objects cannot be deleted through the API; Terraform lets go of it
  # instead of failing the destroy.
  deletion_policy = "ABANDON"
}

module "dsn" {
  source   = "../secret"
  for_each = local.dsn_secrets

  project_id = var.project_id
  region     = var.region
  secret_id  = lower(replace(each.key, "_", "-"))
  labels     = var.labels

  mode          = "generated"
  value_version = var.credentials_version
  value_wo      = "postgres://${local.roles[each.value.role]}:${ephemeral.random_password.role[each.value.role].result}@${google_sql_database_instance.this.private_ip_address}:5432/${var.database_name}?sslmode=require"

  accessor_members = each.value.accessors
}
