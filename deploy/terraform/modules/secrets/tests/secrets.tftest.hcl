# Offline: google is mocked, nothing here reaches Google Cloud. random is the real provider
# because Terraform's mock providers cannot yet stand in for ephemeral resources, and random
# only computes locally.
mock_provider "google" {}

variables {
  project_id       = "kiwi-test-123"
  accessor_members = ["serviceAccount:kiwi-run@kiwi-test-123.iam.gserviceaccount.com"]
}

run "one_secret_per_variable_named_after_it" {
  command = apply

  assert {
    condition = toset(keys(output.secret_ids)) == toset([
      "KIWI_OAUTH_CLIENT_ID", "KIWI_OAUTH_CLIENT_SECRET", "KIWI_TOKEN_ENC_KEY", "KIWI_BLOB_SIGNING_KEY",
    ])
    error_message = "the module must publish exactly the four application secrets: ${jsonencode(keys(output.secret_ids))}"
  }
  assert {
    condition     = output.secret_ids["KIWI_OAUTH_CLIENT_SECRET"] == "kiwi-oauth-client-secret" && output.secret_ids["KIWI_BLOB_SIGNING_KEY"] == "kiwi-blob-signing-key"
    error_message = "a secret is named after its variable: lowercase, hyphens"
  }
}

run "the_oauth_client_is_added_by_hand" {
  command = apply

  assert {
    condition     = toset(keys(output.external_secret_ids)) == toset(["KIWI_OAUTH_CLIENT_ID", "KIWI_OAUTH_CLIENT_SECRET"])
    error_message = "the Google OAuth client cannot be created by Terraform: its id and secret are external secrets"
  }
}

run "no_value_is_in_any_output" {
  command = apply

  assert {
    condition     = alltrue([for id in values(output.secret_ids) : can(regex("^kiwi-[a-z-]+$", id))])
    error_message = "outputs carry secret ids only"
  }
}

run "an_environment_variable_cannot_be_both_external_and_generated" {
  command = plan

  variables {
    external_secrets = ["KIWI_TOKEN_ENC_KEY"]
  }

  expect_failures = [var.generated_keys]
}
