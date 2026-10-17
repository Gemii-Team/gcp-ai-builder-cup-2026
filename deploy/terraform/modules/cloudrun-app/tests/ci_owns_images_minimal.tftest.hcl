# CI owns the images once the first apply is done: a later apply with another image value must not
# roll a deployed image back or forward. This is the one test here that plans against an existing
# state (the first run applies, the second plans), so it has a file of its own: runs of one file
# share state. Offline: google is mocked, nothing here reaches Google Cloud.
#
# The minimal profile has ONE service container, kiwi, at containers[0]; the lifecycle block lists
# both container indexes because ignore_changes cannot depend on a variable. This proves that the
# index that does not exist there breaks nothing and that the one that does is ignored.
mock_provider "google" {}

variables {
  project_id                 = "kiwi-test-123"
  run_service_account_email  = "kiwi-run@kiwi-test-123.iam.gserviceaccount.com"
  jobs_service_account_email = "kiwi-jobs@kiwi-test-123.iam.gserviceaccount.com"
  network_name               = "kiwi"
  subnet_name                = "kiwi-run"
  public_url                 = "https://kiwi-app-123456789012.asia-southeast1.run.app"
  blob_url                   = "gs://kiwi-test-123-kiwi-uploads"
  single_container           = true
  valkey_url                 = null
  runtime_secret_ids         = { "KIWI_DATABASE_URL" = "kiwi-database-url" }
  owner_secret_ids           = { "KIWI_MIGRATE_DATABASE_URL" = "kiwi-migrate-database-url" }
  readonly_secret_ids        = { "KIWI_DATABASE_RO_URL" = "kiwi-database-ro-url" }
  kiwi_image                 = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci"
}

run "first_apply_creates_the_service_and_the_jobs" {
  command = apply
}

run "a_later_apply_with_another_image_changes_none_of_them" {
  command = plan

  variables {
    kiwi_image = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:other"
  }

  assert {
    condition     = google_cloud_run_v2_service.this.template[0].containers[0].image == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci"
    error_message = "the service image must stay what CI deployed"
  }
  assert {
    condition     = google_cloud_run_v2_job.migrate.template[0].template[0].containers[0].image == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci" && google_cloud_run_v2_job.seed.template[0].template[0].containers[0].image == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci"
    error_message = "the Jobs' image must stay what CI deployed"
  }
}
