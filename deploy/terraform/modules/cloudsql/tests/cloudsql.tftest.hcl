# Offline: google is mocked, nothing here reaches Google Cloud. random is the real provider
# because Terraform's mock providers cannot yet stand in for ephemeral resources, and random
# only computes locally.
mock_provider "google" {}

variables {
  project_id                  = "kiwi-test-123"
  network_id                  = "projects/kiwi-test-123/global/networks/kiwi"
  private_services_range_name = "kiwi-private-services"
  runtime_members             = ["serviceAccount:kiwi-run@kiwi-test-123.iam.gserviceaccount.com"]
  bootstrap_members           = ["serviceAccount:kiwi-jobs@kiwi-test-123.iam.gserviceaccount.com"]
}

run "defaults_are_the_chosen_instance" {
  command = plan

  assert {
    condition     = google_sql_database_instance.this.settings[0].tier == "db-g1-small"
    error_message = "the default tier must be db-g1-small"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].edition == "ENTERPRISE"
    error_message = "the edition must be set explicitly to ENTERPRISE (PostgreSQL 17 otherwise defaults to Enterprise Plus)"
  }
  assert {
    condition     = google_sql_database_instance.this.database_version == "POSTGRES_17"
    error_message = "the default engine must be PostgreSQL 17"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].availability_type == "ZONAL"
    error_message = "the instance must be zonal"
  }
}

run "private_ip_only_and_tls_required" {
  command = plan

  assert {
    condition     = google_sql_database_instance.this.settings[0].ip_configuration[0].ipv4_enabled == false
    error_message = "the instance must have no public IPv4 address"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].ip_configuration[0].private_network == var.network_id
    error_message = "the instance must be attached to the VPC for private IP"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].ip_configuration[0].ssl_mode == "ENCRYPTED_ONLY"
    error_message = "every connection must be encrypted"
  }
  assert {
    condition     = length(google_sql_database_instance.this.settings[0].ip_configuration[0].authorized_networks) == 0
    error_message = "no authorized network may be configured"
  }
}

run "protected_and_backed_up" {
  command = plan

  assert {
    condition     = google_sql_database_instance.this.deletion_protection == true
    error_message = "Terraform must refuse to destroy the instance"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].deletion_protection_enabled == true
    error_message = "Google Cloud must refuse to delete the instance (API, gcloud, console)"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].backup_configuration[0].enabled == true
    error_message = "automated backups must be on"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].backup_configuration[0].point_in_time_recovery_enabled == true
    error_message = "point-in-time recovery must be on by default"
  }
}

run "max_connections_holds_two_overlapping_instances" {
  command = plan

  assert {
    condition     = one([for f in google_sql_database_instance.this.settings[0].database_flags : f.value if f.name == "max_connections"]) == "100"
    error_message = "max_connections must be 100: two overlapping instances at 20 + 16 connections each, plus Jobs and reserved slots"
  }
}

run "max_connections_can_be_left_to_the_tier" {
  command = plan

  variables {
    max_connections = null
  }

  assert {
    condition     = length([for f in google_sql_database_instance.this.settings[0].database_flags : f if f.name == "max_connections"]) == 0
    error_message = "with max_connections null the max_connections flag must not be set"
  }
  assert {
    condition     = length(google_sql_database_instance.this.settings[0].database_flags) == 5
    error_message = "the five logging flags stay"
  }
}

run "max_connections_below_the_minimum_is_refused" {
  command = plan

  variables {
    max_connections = 5
  }

  expect_failures = [var.max_connections]
}

run "shared_core_tier_needs_enterprise_edition" {
  command = plan

  variables {
    edition = "ENTERPRISE_PLUS"
  }

  expect_failures = [google_sql_database_instance.this]
}

# What the minimal profile's live unit passes (live/root.hcl): db-f1-micro, the project owner's
# decision of 2026-10-14, and max_connections 50, above the 25 that tier starts with (the flag is
# not capped per tier; nobody has tried it on a real instance). The connection budget itself
# (pools against this flag) is recomputed from the rendered units by scripts/iac/check.sh.
run "minimal_profile_is_db_f1_micro_in_the_enterprise_edition_with_50_connections" {
  command = plan

  variables {
    tier            = "db-f1-micro"
    max_connections = 50
  }

  assert {
    condition     = google_sql_database_instance.this.settings[0].tier == "db-f1-micro"
    error_message = "the minimal profile is db-f1-micro (decision of 2026-10-14; the way back is db-g1-small)"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].edition == "ENTERPRISE"
    error_message = "a shared-core tier exists only in the ENTERPRISE edition"
  }
  assert {
    condition     = one([for f in google_sql_database_instance.this.settings[0].database_flags : f.value if f.name == "max_connections"]) == "50"
    error_message = "max_connections must be pinned at 50, the budget the minimal profile's pools were sized for (db-f1-micro starts at 25)"
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].availability_type == "ZONAL" && google_sql_database_instance.this.settings[0].ip_configuration[0].ipv4_enabled == false
    error_message = "zonal and private IP only in every profile"
  }
}

run "db_f1_micro_is_accepted_only_in_the_enterprise_edition" {
  command = plan

  variables {
    tier    = "db-f1-micro"
    edition = "ENTERPRISE_PLUS"
  }

  expect_failures = [google_sql_database_instance.this]
}

run "only_the_owner_is_a_cloud_sql_user_and_its_password_stays_out_of_state" {
  command = plan

  assert {
    condition     = google_sql_user.owner.name == "kiwi_owner"
    error_message = "the table owner login must be kiwi_owner"
  }
  assert {
    condition     = google_sql_user.owner.password == null
    error_message = "the owner password must be set through the write-only argument, never through password"
  }
  assert {
    condition     = google_sql_user.owner.password_wo_version == 1
    error_message = "the write-only password needs a version so it can be rotated"
  }
  assert {
    condition     = google_sql_user.owner.deletion_policy == "ABANDON"
    error_message = "a Postgres user that owns objects cannot be deleted through the API"
  }
}

run "connection_strings_are_published_by_secret_id_only" {
  command = apply

  assert {
    condition     = keys(output.owner_secret_ids) == ["KIWI_MIGRATE_DATABASE_URL"]
    error_message = "the owner's connection string must be its own output, for the migrate Job only"
  }
  assert {
    condition     = keys(output.runtime_secret_ids) == ["KIWI_DATABASE_SYSTEM_URL", "KIWI_DATABASE_URL"]
    error_message = "the runtime outputs must be the app and system strings and nothing else"
  }
  assert {
    condition     = keys(output.readonly_secret_ids) == ["KIWI_DATABASE_RO_URL"]
    error_message = "the read-only string must be its own output"
  }
  assert {
    condition = alltrue([
      for id in concat(values(output.owner_secret_ids), values(output.runtime_secret_ids), values(output.readonly_secret_ids)) :
      can(regex("^kiwi-[a-z-]+-url$", id))
    ])
    error_message = "outputs must carry secret ids, never connection strings"
  }
  assert {
    condition     = !can(regex("postgres://", jsonencode([output.instance_name, output.connection_name, output.private_ip_address, output.database_name, output.owner_secret_ids, output.runtime_secret_ids, output.readonly_secret_ids])))
    error_message = "no connection string may appear in any output"
  }
}
