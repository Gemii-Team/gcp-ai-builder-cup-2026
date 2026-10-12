# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}

variables {
  project_id           = "kiwi-test-123"
  cors_origins         = ["https://kiwi-app-123456789012.asia-southeast1.run.app"]
  object_admin_members = ["serviceAccount:kiwi-run@kiwi-test-123.iam.gserviceaccount.com"]
}

run "bucket_is_never_public" {
  command = plan

  assert {
    condition     = google_storage_bucket.uploads.public_access_prevention == "enforced"
    error_message = "public access prevention must be enforced"
  }
  assert {
    condition     = google_storage_bucket.uploads.uniform_bucket_level_access == true
    error_message = "uniform bucket-level access must be on: no object ACLs"
  }
  assert {
    condition     = google_storage_bucket.uploads.force_destroy == false
    error_message = "destroying the stack must not delete users' documents"
  }
}

run "browser_access_is_from_the_app_origin_only" {
  command = plan

  assert {
    condition     = google_storage_bucket.uploads.cors[0].origin == var.cors_origins && !contains(google_storage_bucket.uploads.cors[0].origin, "*")
    error_message = "CORS must list exactly the app's origin, never a wildcard"
  }
  assert {
    condition     = toset(google_storage_bucket.uploads.cors[0].method) == toset(["GET", "HEAD", "PUT"])
    error_message = "browsers PUT uploads and GET downloads through signed URLs, nothing else"
  }
}

run "wildcard_origin_is_refused" {
  command = plan

  variables {
    cors_origins = ["*"]
  }

  expect_failures = [var.cors_origins]
}

run "http_origin_is_refused" {
  command = plan

  variables {
    cors_origins = ["http://kiwi.example.com"]
  }

  expect_failures = [var.cors_origins]
}

run "abandoned_uploads_are_deleted_by_a_lifecycle_rule" {
  command = plan

  assert {
    condition     = one(one(google_storage_bucket.uploads.lifecycle_rule).action).type == "Delete"
    error_message = "the lifecycle rule must delete"
  }
  assert {
    condition     = contains(tolist(one(one(google_storage_bucket.uploads.lifecycle_rule).condition).matches_prefix), "uploads/") && tonumber(one(one(google_storage_bucket.uploads.lifecycle_rule).condition).age) == 7
    error_message = "the rule must apply to uploads/ after 7 days by default"
  }
}

run "object_admin_is_granted_on_this_bucket_to_the_listed_members_only" {
  command = plan

  assert {
    condition     = toset(keys(google_storage_bucket_iam_member.object_admin)) == toset(var.object_admin_members)
    error_message = "exactly the listed members get a grant"
  }
  assert {
    condition     = alltrue([for m in google_storage_bucket_iam_member.object_admin : m.role == "roles/storage.objectAdmin"])
    error_message = "the role is storage.objectAdmin on the bucket, not a project role"
  }
}

run "bucket_name_defaults_to_the_project_id_and_uploads" {
  command = plan

  assert {
    condition     = google_storage_bucket.uploads.name == "kiwi-test-123-kiwi-uploads"
    error_message = "the default bucket name is <project>-kiwi-uploads"
  }
}

run "deleted_and_overwritten_objects_are_recoverable_for_seven_days" {
  command = plan

  assert {
    condition     = one(google_storage_bucket.uploads.soft_delete_policy).retention_duration_seconds == 604800
    error_message = "versioning is off on purpose: soft delete must keep deleted and overwritten objects for seven days"
  }
}
