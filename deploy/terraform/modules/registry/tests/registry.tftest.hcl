# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}

variables {
  project_id     = "kiwi-test-123"
  writer_members = ["serviceAccount:kiwi-deploy@kiwi-test-123.iam.gserviceaccount.com"]
}

run "docker_repository_in_the_service_region" {
  command = plan

  assert {
    condition     = google_artifact_registry_repository.this.format == "DOCKER" && google_artifact_registry_repository.this.location == "asia-southeast1"
    error_message = "the repository is a Docker repository in the region of Cloud Run"
  }
  assert {
    condition     = output.repository_path == "asia-southeast1-docker.pkg.dev/kiwi-test-123/kiwi"
    error_message = "the registry path must be <region>-docker.pkg.dev/<project>/<repository>, got ${output.repository_path}"
  }
}

run "old_images_are_cleaned_up_and_the_recent_ones_kept" {
  command = plan

  assert {
    condition     = google_artifact_registry_repository.this.cleanup_policy_dry_run == false
    error_message = "the cleanup policies must be live, not a dry run"
  }
  assert {
    condition     = length(google_artifact_registry_repository.this.cleanup_policies) == 2
    error_message = "one rule deletes untagged images, one keeps the recent versions"
  }
}

run "only_the_listed_members_may_push_and_on_this_repository_only" {
  command = plan

  assert {
    condition     = toset(keys(google_artifact_registry_repository_iam_member.writer)) == toset(var.writer_members)
    error_message = "exactly the listed members get a grant"
  }
  assert {
    condition     = alltrue([for m in google_artifact_registry_repository_iam_member.writer : m.role == "roles/artifactregistry.writer"])
    error_message = "pushing is roles/artifactregistry.writer on the repository"
  }
}

run "keeping_fewer_than_two_versions_is_refused" {
  command = plan

  variables {
    keep_recent_versions = 1
  }

  expect_failures = [var.keep_recent_versions]
}
