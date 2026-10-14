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

    # max_connections
    #
    # === The "full" profile: db-g1-small, max_connections 100 ===================================
    # The app opens up to 20 (kiwi_app) + 16 (kiwi_system) connections per process unless told
    # otherwise: those are its built-in sizes. Cloud SQL's default max_connections follows memory:
    # 25 on db-f1-micro, 50 on db-g1-small, 100 from 3.75 GB. One instance needs 36 of them. A
    # rollout keeps the old revision alive for as long as its WebSockets stay open (up to the
    # 60-minute request timeout), so two instances need 72, and the migrate and seed Jobs, Cloud
    # SQL's own agents and the reserved superuser slots come on top.
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
    # === The "minimal" profile: db-g1-small, max_connections 50, pools of 6 + 6 ================
    # The live unit passes tier = db-g1-small and max_connections = 50 (the default of that tier,
    # pinned so a change of Cloud SQL's defaults cannot shrink it silently) and the app gets
    # KIWI_DB_APP_MAX_CONNS = 6, KIWI_DB_SYSTEM_MAX_CONNS = 6 and KIWI_DB_MAX_CONN_IDLE = 2m
    # (architecture review F10). 6 is the lowest system pool the app accepts: the outbox relay
    # holds 4 system connections while jobs run, plus 2 of headroom.
    #
    # What Google documents, read 2026-10-14:
    #  - pgvector: "PostgreSQL versions 13 and later support version 0.8.5", so PostgreSQL 17 has
    #    it, and the page restricts no extension by machine type, shared-core included. Extensions
    #    are created by members of cloudsqlsuperuser (the owner is one).
    #    https://docs.cloud.google.com/sql/docs/postgres/extensions
    #  - Memory: db-f1-micro 0.614 GB, db-g1-small 1.7 GB.
    #    https://docs.cloud.google.com/sql/docs/postgres/machine-series-overview
    #  - Shared-core machine types exist in the Enterprise edition only, and PostgreSQL 16 and
    #    later default to Enterprise Plus, so the edition is always set (see the precondition).
    #    https://docs.cloud.google.com/sql/docs/postgres/editions-intro
    #  - max_connections defaults: tiny (~0.5 GB) 25, small (~1.7 GB) 50, from 3.75 GB 100. The
    #    range is 14 to "varies" and nothing there ties it to a tier, so the flag CAN be raised on
    #    db-f1-micro. shared_buffers defaults to a third of the instance's memory.
    #    https://docs.cloud.google.com/sql/docs/postgres/flags
    #  - Shared-core and single-zone instances are outside the Cloud SQL SLA: expect no
    #    availability guarantee on either tier.
    #    https://docs.cloud.google.com/sql/docs/postgres/operational-guidelines
    #
    # Connection math, the ceiling and not the usual case (a pool opens connections when it needs
    # them and closes idle ones after 2m, so the old revision's pools shrink while it lingers):
    #
    #   the service, two revisions alive in a rollout   2 x (6 app + 6 system)    = 24
    #   the seed Job, its own pools                         6 app + 6 system      = 12
    #   the migrate Job, the owner's connection             goose, one at a time  =  2
    #   reserved: PostgreSQL superuser_reserved_connections (default 3), Cloud SQL's
    #     own agents and one operator's psql session    [INFERENCE: no Google page    =  8
    #                                                    gives the agents' count]
    #                                                                       total  = 46 <= 50
    #
    # scripts/iac/check.sh recomputes this from the rendered inputs of the cloudrun and cloudsql
    # units and fails when the sum exceeds max_connections. Raising a pool without raising the
    # flag (and checking memory) fails the check; the other way round (flag up, pools unchanged)
    # passes.
    #
    # Why db-g1-small and not db-f1-micro. The flag could be raised to 50 on db-f1-micro, so the
    # connection count is not what decides it: memory is. 0.614 GB with shared_buffers at a third
    # (about 205 MB) leaves about 410 MB for the operating system, Cloud SQL's agents and every
    # backend, and a backend that runs the row-security policies, a sort or an HNSW scan holds
    # several MB of its own [INFERENCE: typical PostgreSQL behaviour, not measured here]. An
    # out-of-memory kill restarts the instance and drops every connection in the middle of a demo,
    # to save about $39.51 over 18 Oct - 4 Dec (Singapore list price, 1,152 h x ($0.0490 - $0.0147)).
    # db-g1-small (1.7 GB, shared_buffers about 580 MB) leaves about 1.1 GB.
    #
    # Memory risk that remains on db-g1-small, and what to watch. Nothing here was measured. If the
    # instance's memory metric stays above about 80% or it restarts, move to db-custom-1-3840
    # (3.75 GB, default max_connections 100) before touching the pools. Shared-core CPU is
    # burstable, and neither tier has an SLA.
    #
    # The flag restarts the instance when it changes. Pool sizes live in
    # deploy/terraform/live/root.hcl.
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
