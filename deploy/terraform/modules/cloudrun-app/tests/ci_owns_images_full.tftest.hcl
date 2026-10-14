# The full profile's counterpart of ci_owns_images_minimal.tftest.hcl: web at containers[0], kiwi at
# containers[1], both images owned by CI after the first apply. Own file, because runs of one file
# share state. Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}

variables {
  project_id                 = "kiwi-test-123"
  run_service_account_email  = "kiwi-run@kiwi-test-123.iam.gserviceaccount.com"
  jobs_service_account_email = "kiwi-jobs@kiwi-test-123.iam.gserviceaccount.com"
  network_name               = "kiwi"
  subnet_name                = "kiwi-run"
  public_url                 = "https://kiwi-app-123456789012.asia-southeast1.run.app"
  blob_url                   = "gs://kiwi-test-123-kiwi-uploads"
  valkey_url                 = "redis://10.20.1.2:6379"
  runtime_secret_ids         = { "KIWI_DATABASE_URL" = "kiwi-database-url" }
  owner_secret_ids           = { "KIWI_MIGRATE_DATABASE_URL" = "kiwi-migrate-database-url" }
  readonly_secret_ids        = { "KIWI_DATABASE_RO_URL" = "kiwi-database-ro-url" }
  web_image                  = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/web:deployed-by-ci"
  kiwi_image                 = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci"
}

run "first_apply_creates_the_service_and_the_jobs" {
  command = apply
}

run "a_later_apply_with_other_images_changes_none_of_them" {
  command = plan

  variables {
    web_image  = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/web:other"
    kiwi_image = "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:other"
  }

  assert {
    condition     = google_cloud_run_v2_service.this.template[0].containers[0].image == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/web:deployed-by-ci" && google_cloud_run_v2_service.this.template[0].containers[1].image == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci"
    error_message = "both service images must stay what CI deployed"
  }
  assert {
    condition     = google_cloud_run_v2_job.migrate.template[0].template[0].containers[0].image == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci" && google_cloud_run_v2_job.seed.template[0].template[0].containers[0].image == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi/kiwi:deployed-by-ci"
    error_message = "the Jobs' image must stay what CI deployed"
  }
}
