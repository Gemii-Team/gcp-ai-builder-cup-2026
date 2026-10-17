# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}

variables {
  project_id       = "kiwi-test-123"
  region           = "asia-southeast1"
  secret_id        = "kiwi-token-enc-key"
  value_wo         = "test-value-that-is-not-a-secret"
  accessor_members = ["serviceAccount:kiwi-run@kiwi-test-123.iam.gserviceaccount.com"]
}

run "generated_value_goes_through_the_write_only_argument" {
  command = plan

  assert {
    condition     = length(google_secret_manager_secret_version.generated) == 1 && length(google_secret_manager_secret_version.placeholder) == 0
    error_message = "a generated secret has the generated version and no placeholder"
  }
  assert {
    condition     = google_secret_manager_secret_version.generated[0].secret_data == null
    error_message = "secret_data would store the value in state: it must stay null, the value goes through secret_data_wo"
  }
  assert {
    condition     = tonumber(google_secret_manager_secret_version.generated[0].secret_data_wo_version) == 1
    error_message = "the write-only value needs a version so it can be rotated"
  }
}

run "external_secret_gets_a_placeholder_so_the_first_apply_can_start_a_revision" {
  command = plan

  variables {
    mode      = "external"
    value_wo  = null
    secret_id = "kiwi-oauth-client-secret"
  }

  assert {
    condition     = length(google_secret_manager_secret_version.placeholder) == 1 && length(google_secret_manager_secret_version.generated) == 0
    error_message = "an external secret has the placeholder version and no generated one"
  }
  assert {
    condition     = nonsensitive(google_secret_manager_secret_version.placeholder[0].secret_data == null) && tonumber(google_secret_manager_secret_version.placeholder[0].secret_data_wo_version) == 1
    error_message = "the placeholder is written once, through the write-only argument"
  }
}

run "secret_is_pinned_to_one_region_and_protected" {
  command = plan

  assert {
    condition     = one(google_secret_manager_secret.this.replication[0].user_managed[0].replicas).location == "asia-southeast1"
    error_message = "the secret must be replicated to the one region of the stack"
  }
  assert {
    condition     = google_secret_manager_secret.this.deletion_protection == true
    error_message = "losing a secret can make stored data unreadable: deletion protection must be on by default"
  }
}

run "accessors_are_granted_on_this_secret_only_and_read_only" {
  command = plan

  assert {
    condition     = toset(keys(google_secret_manager_secret_iam_member.accessor)) == toset(var.accessor_members)
    error_message = "exactly the listed members get a grant"
  }
  assert {
    condition     = alltrue([for m in google_secret_manager_secret_iam_member.accessor : m.role == "roles/secretmanager.secretAccessor"])
    error_message = "the grant is secretAccessor (read a version), never admin"
  }
}

run "nobody_is_granted_anything_by_default" {
  command = plan

  variables {
    accessor_members = []
  }

  assert {
    condition     = length(google_secret_manager_secret_iam_member.accessor) == 0
    error_message = "with no members no grant may exist"
  }
}

run "mode_must_be_generated_or_external" {
  command = plan

  variables {
    mode = "plaintext"
  }

  expect_failures = [var.mode]
}

run "the_value_is_not_in_the_output" {
  command = apply

  assert {
    condition     = output.secret_id == "kiwi-token-enc-key"
    error_message = "the output is the secret's id, never its value"
  }
}
