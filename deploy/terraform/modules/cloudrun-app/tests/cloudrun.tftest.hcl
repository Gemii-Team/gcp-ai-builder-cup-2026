# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}

variables {
  project_id                 = "kiwi-test-123"
  run_service_account_email  = "kiwi-run@kiwi-test-123.iam.gserviceaccount.com"
  jobs_service_account_email = "kiwi-jobs@kiwi-test-123.iam.gserviceaccount.com"
  deploy_member              = "serviceAccount:kiwi-deploy@kiwi-test-123.iam.gserviceaccount.com"
  network_name               = "kiwi"
  subnet_name                = "kiwi-run"
  public_url                 = "https://kiwi-app-123456789012.asia-southeast1.run.app"
  blob_url                   = "gs://kiwi-test-123-kiwi-uploads"
  valkey_url                 = "redis://10.20.1.2:6379"
  runtime_secret_ids = {
    "KIWI_DATABASE_URL"        = "kiwi-database-url"
    "KIWI_DATABASE_SYSTEM_URL" = "kiwi-database-system-url"
    "KIWI_OAUTH_CLIENT_ID"     = "kiwi-oauth-client-id"
    "KIWI_OAUTH_CLIENT_SECRET" = "kiwi-oauth-client-secret"
    "KIWI_TOKEN_ENC_KEY"       = "kiwi-token-enc-key"
    "KIWI_BLOB_SIGNING_KEY"    = "kiwi-blob-signing-key"
  }
  owner_secret_ids    = { "KIWI_MIGRATE_DATABASE_URL" = "kiwi-migrate-database-url" }
  readonly_secret_ids = { "KIWI_DATABASE_RO_URL" = "kiwi-database-ro-url" }
}

run "one_instance_always_on_gen2_with_an_hour_timeout" {
  command = plan

  assert {
    condition     = google_cloud_run_v2_service.this.template[0].scaling[0].min_instance_count == 1 && google_cloud_run_v2_service.this.template[0].scaling[0].max_instance_count == 1
    error_message = "min = max = 1: the bus is in-process, two instances would each run the relay and the poll"
  }
  assert {
    condition     = alltrue([for c in google_cloud_run_v2_service.this.template[0].containers : c.resources[0].cpu_idle == false])
    error_message = "CPU must be always allocated (cpu_idle false) for every container: the relay and the poll run between requests"
  }
  assert {
    condition     = google_cloud_run_v2_service.this.template[0].timeout == "3600s"
    error_message = "the request timeout must be 3600 s: a WebSocket lives as long as its request"
  }
  assert {
    condition     = google_cloud_run_v2_service.this.template[0].execution_environment == "EXECUTION_ENVIRONMENT_GEN2"
    error_message = "second-generation execution environment is required (PDF reader sandbox)"
  }
  assert {
    condition     = google_cloud_run_v2_service.this.template[0].max_instance_request_concurrency == 1000
    error_message = "every WebSocket is a request: concurrency must be 1000, not the default 80"
  }
}

run "direct_vpc_egress_for_private_ranges_only" {
  command = plan

  assert {
    condition     = google_cloud_run_v2_service.this.template[0].vpc_access[0].egress == "PRIVATE_RANGES_ONLY"
    error_message = "only private ranges go through the VPC: Vertex AI, Cloud Storage and Google sign-in must leave Cloud Run directly"
  }
  assert {
    condition     = one(google_cloud_run_v2_service.this.template[0].vpc_access[0].network_interfaces).subnetwork == "kiwi-run"
    error_message = "Direct VPC egress must use the Cloud Run subnet"
  }
  assert {
    condition     = length(google_cloud_run_v2_service.this.template[0].vpc_access[0].connector) == 0 || google_cloud_run_v2_service.this.template[0].vpc_access[0].connector == null
    error_message = "no Serverless VPC Access connector: Direct VPC egress"
  }
}

run "two_containers_web_ingress_and_kiwi_sidecar" {
  command = plan

  assert {
    condition     = toset([for c in google_cloud_run_v2_service.this.template[0].containers : c.name]) == toset(["web", "kiwi"])
    error_message = "the service has exactly the ingress container and the kiwi sidecar"
  }
  assert {
    condition     = one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "web"]).ports[0].container_port == 8080
    error_message = "the ingress container takes traffic on its port"
  }
  assert {
    condition     = one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).resources[0].limits["memory"] == "2Gi"
    error_message = "kiwi needs at least 2 GiB"
  }
}

run "kiwi_probes_hit_the_sidecar_not_the_ingress" {
  command = plan

  assert {
    condition     = one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).startup_probe[0].http_get[0].path == "/readyz" && one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).startup_probe[0].http_get[0].port == 8081
    error_message = "startup is /readyz on the kiwi port (Postgres and Valkey reachable)"
  }
  assert {
    condition     = one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).liveness_probe[0].http_get[0].path == "/healthz"
    error_message = "liveness is /healthz on the kiwi port"
  }
}

run "the_sidecar_runs_in_prod_with_vertex_and_a_memory_limit" {
  command = plan

  assert {
    condition = {
      for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name => e.value
    }["KIWI_ENV"] == "prod"
    error_message = "KIWI_ENV must be prod"
  }
  assert {
    condition = {
      for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name => e.value
    }["KIWI_PUBLIC_URL"] == "https://kiwi-app-123456789012.asia-southeast1.run.app"
    error_message = "KIWI_PUBLIC_URL must be the origin given"
  }
  assert {
    condition = {
      for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name => e.value
    }["GOMEMLIMIT"] == "1638MiB"
    error_message = "GOMEMLIMIT must be 80% of the 2 GiB container"
  }
}

run "vertex_defaults_are_singapore_flash_and_embedding_001_without_global" {
  command = plan

  assert {
    condition = {
      for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name => e.value
    }["KIWI_LLM_MODEL_FLASH"] == "gemini-3.5-flash"
    error_message = "the default generation model is gemini-3.5-flash"
  }
  assert {
    condition = {
      for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name => e.value
    }["KIWI_VERTEX_LOCATION"] == "asia-southeast1"
    error_message = "the default Vertex location is asia-southeast1"
  }
  assert {
    condition     = !contains([for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name], "KIWI_VERTEX_ALLOW_GLOBAL")
    error_message = "the global endpoint must not be allowed unless the location is global"
  }
}

run "choosing_the_global_endpoint_is_one_variable_and_opts_in_explicitly" {
  command = plan

  variables {
    vertex_location = "global"
  }

  assert {
    condition = {
      for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name => e.value
    }["KIWI_VERTEX_ALLOW_GLOBAL"] == "true"
    error_message = "a global location must set KIWI_VERTEX_ALLOW_GLOBAL so residency is never lost silently"
  }
}

run "embeddings_cannot_be_pointed_at_the_global_endpoint" {
  command = plan

  variables {
    vertex_embedding_location = "global"
  }

  expect_failures = [var.vertex_embedding_location]
}

run "the_service_never_receives_the_owner_or_readonly_connection_string" {
  command = plan

  assert {
    condition     = !contains([for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name], "KIWI_MIGRATE_DATABASE_URL") && !contains([for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).env : e.name], "KIWI_DATABASE_RO_URL")
    error_message = "the owner credential defeats row-level security: it must not reach the serving container"
  }
  assert {
    condition     = !contains([for e in one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "web"]).env : e.name], "KIWI_DATABASE_URL")
    error_message = "the ingress container must hold no database credential"
  }
}

run "only_the_migrate_job_receives_the_owner_connection_string" {
  command = plan

  assert {
    condition     = contains([for e in google_cloud_run_v2_job.migrate.template[0].template[0].containers[0].env : e.name], "KIWI_MIGRATE_DATABASE_URL")
    error_message = "the migrate Job needs the owner's string"
  }
  assert {
    condition     = !contains([for e in google_cloud_run_v2_job.seed.template[0].template[0].containers[0].env : e.name], "KIWI_MIGRATE_DATABASE_URL")
    error_message = "the seed Job runs the serving configuration and must not hold the owner's string"
  }
}

run "jobs_run_once_without_retries_as_the_jobs_identity" {
  command = plan

  assert {
    condition     = google_cloud_run_v2_job.migrate.template[0].parallelism == 1 && google_cloud_run_v2_job.migrate.template[0].task_count == 1 && google_cloud_run_v2_job.migrate.template[0].template[0].max_retries == 0
    error_message = "the migrate Job: one task, parallelism 1, no retries"
  }
  assert {
    condition     = google_cloud_run_v2_job.seed.template[0].parallelism == 1 && google_cloud_run_v2_job.seed.template[0].task_count == 1 && google_cloud_run_v2_job.seed.template[0].template[0].max_retries == 0
    error_message = "the seed Job: one task, parallelism 1, no retries"
  }
  assert {
    condition     = google_cloud_run_v2_job.migrate.template[0].template[0].service_account == var.jobs_service_account_email && google_cloud_run_v2_job.seed.template[0].template[0].service_account == var.jobs_service_account_email
    error_message = "both Jobs run as the jobs identity"
  }
  assert {
    condition     = google_cloud_run_v2_service.this.template[0].service_account == var.run_service_account_email
    error_message = "the service runs as the runtime identity"
  }
  assert {
    condition     = join(" ", google_cloud_run_v2_job.migrate.template[0].template[0].containers[0].args) == "migrate" && join(" ", google_cloud_run_v2_job.seed.template[0].template[0].containers[0].args) == "seed"
    error_message = "the Jobs run the kiwi subcommands migrate and seed"
  }
}

run "first_apply_needs_no_app_image" {
  command = plan

  assert {
    condition     = one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).image == "us-docker.pkg.dev/cloudrun/container/hello"
    error_message = "with no image given the sidecar starts from Google's public hello image"
  }
  assert {
    condition     = google_cloud_run_v2_job.migrate.template[0].template[0].containers[0].image == "gcr.io/distroless/static:nonroot"
    error_message = "a placeholder Job must fail at once when executed, so its image has no program"
  }
}

run "given_images_are_used_by_the_sidecar_and_both_jobs" {
  command = plan

  variables {
    web_image  = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/web:abc"
    kiwi_image = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:abc"
  }

  assert {
    condition     = one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "kiwi"]).image == var.kiwi_image && one([for c in google_cloud_run_v2_service.this.template[0].containers : c if c.name == "web"]).image == var.web_image
    error_message = "the service containers use the images given"
  }
  assert {
    condition     = google_cloud_run_v2_job.migrate.template[0].template[0].containers[0].image == var.kiwi_image && google_cloud_run_v2_job.seed.template[0].template[0].containers[0].image == var.kiwi_image
    error_message = "both Jobs run the kiwi image"
  }
}

run "the_service_is_public_and_the_jobs_never_are" {
  command = plan

  assert {
    condition     = length(google_cloud_run_v2_service_iam_member.public) == 1 && google_cloud_run_v2_service_iam_member.public[0].member == "allUsers" && google_cloud_run_v2_service_iam_member.public[0].role == "roles/run.invoker"
    error_message = "the app authenticates its own users: allUsers is the invoker of the service"
  }
  assert {
    condition     = alltrue([for m in google_cloud_run_v2_job_iam_member.deployer : m.member != "allUsers"])
    error_message = "no Job may be public"
  }
}

run "public_access_can_be_withdrawn" {
  command = plan

  variables {
    allow_public_access = false
  }

  assert {
    condition     = length(google_cloud_run_v2_service_iam_member.public) == 0
    error_message = "with allow_public_access false there must be no allUsers grant"
  }
}

run "the_deploy_identity_may_deploy_these_resources_only" {
  command = plan

  assert {
    condition     = google_cloud_run_v2_service_iam_member.deployer[0].role == "roles/run.developer" && toset(keys(google_cloud_run_v2_job_iam_member.deployer)) == toset(["migrate", "seed"])
    error_message = "the deploy identity gets run.developer on the service and the two Jobs"
  }
}

run "kiwi_memory_below_two_gib_is_refused" {
  command = plan

  variables {
    kiwi_memory_gib = 1
  }

  expect_failures = [var.kiwi_memory_gib]
}

run "a_public_url_with_a_trailing_slash_is_refused" {
  command = plan

  variables {
    public_url = "https://kiwi-app-123456789012.asia-southeast1.run.app/"
  }

  expect_failures = [var.public_url]
}

run "extra_env_cannot_override_a_managed_variable" {
  command = plan

  variables {
    extra_env = { KIWI_ENV = "local" }
  }

  expect_failures = [google_cloud_run_v2_service.this]
}

run "valkey_url_with_a_credential_is_refused" {
  command = plan

  variables {
    valkey_url = "redis://user:secret@10.20.1.2:6379"
  }

  expect_failures = [var.valkey_url]
}

run "service_is_protected_from_deletion" {
  command = plan

  assert {
    condition     = google_cloud_run_v2_service.this.deletion_protection == true && google_cloud_run_v2_job.migrate.deletion_protection == true && google_cloud_run_v2_job.seed.deletion_protection == true
    error_message = "Terraform must refuse to destroy the service and the Jobs"
  }
}
