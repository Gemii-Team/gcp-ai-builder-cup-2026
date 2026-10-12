# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}

variables {
  project_id        = "kiwi-test-123"
  github_repository = "example-org/example-repo"
}

# Computed names, shaped as Google returns them: the provider validates them even against a mock.
override_resource {
  target = google_service_account.this["run"]
  values = {
    name   = "projects/kiwi-test-123/serviceAccounts/kiwi-run@kiwi-test-123.iam.gserviceaccount.com"
    email  = "kiwi-run@kiwi-test-123.iam.gserviceaccount.com"
    member = "serviceAccount:kiwi-run@kiwi-test-123.iam.gserviceaccount.com"
  }
}

override_resource {
  target = google_service_account.this["jobs"]
  values = {
    name   = "projects/kiwi-test-123/serviceAccounts/kiwi-jobs@kiwi-test-123.iam.gserviceaccount.com"
    email  = "kiwi-jobs@kiwi-test-123.iam.gserviceaccount.com"
    member = "serviceAccount:kiwi-jobs@kiwi-test-123.iam.gserviceaccount.com"
  }
}

override_resource {
  target = google_service_account.this["deploy"]
  values = {
    name   = "projects/kiwi-test-123/serviceAccounts/kiwi-deploy@kiwi-test-123.iam.gserviceaccount.com"
    email  = "kiwi-deploy@kiwi-test-123.iam.gserviceaccount.com"
    member = "serviceAccount:kiwi-deploy@kiwi-test-123.iam.gserviceaccount.com"
  }
}

override_resource {
  target = google_iam_workload_identity_pool.github
  values = {
    name = "projects/123456789012/locations/global/workloadIdentityPools/kiwi-github"
  }
}

override_resource {
  target = google_iam_workload_identity_pool_provider.github
  values = {
    name = "projects/123456789012/locations/global/workloadIdentityPools/kiwi-github/providers/github"
  }
}

# The repository-wide guard: no service account key, no plaintext secret argument, anywhere in
# deploy/terraform (a source scan, because a test cannot name a resource that does not exist).
run "no_service_account_key_and_no_plaintext_secret_anywhere" {
  command = plan

  module {
    source = "../../guards/forbidden-patterns"
  }

  assert {
    condition     = output.files_scanned > 20
    error_message = "the guard scanned only ${output.files_scanned} files: it is looking in the wrong place"
  }
  assert {
    condition     = length(output.offenders.service_account_key) == 0
    error_message = "service account keys are forbidden, found in: ${jsonencode(output.offenders.service_account_key)}"
  }
  assert {
    condition     = length(output.offenders.plaintext_password) == 0 && length(output.offenders.plaintext_secret_data) == 0
    error_message = "secrets must go through write-only arguments, found plaintext in: ${jsonencode(output.offenders)}"
  }
}

run "three_identities_with_the_expected_ids" {
  command = plan

  assert {
    condition     = toset([for sa in google_service_account.this : sa.account_id]) == toset(["kiwi-deploy", "kiwi-jobs", "kiwi-run"])
    error_message = "exactly kiwi-run, kiwi-jobs and kiwi-deploy must exist"
  }
}

run "github_workflows_are_pinned_to_one_repository_and_one_ref" {
  command = apply

  assert {
    condition     = google_iam_workload_identity_pool_provider.github.attribute_condition == "assertion.repository == 'example-org/example-repo' && assertion.ref == 'refs/heads/main'"
    error_message = "the provider condition must pin both the repository and refs/heads/main"
  }
  assert {
    condition     = google_iam_workload_identity_pool_provider.github.oidc[0].issuer_uri == "https://token.actions.githubusercontent.com"
    error_message = "the provider must trust GitHub Actions' issuer"
  }
  assert {
    condition     = endswith(google_service_account_iam_member.github_acts_as_deploy.member, "/attribute.repository/example-org/example-repo") && startswith(google_service_account_iam_member.github_acts_as_deploy.member, "principalSet://")
    error_message = "only identities of the pinned repository may act as the deploy account"
  }
  assert {
    condition     = google_service_account_iam_member.github_acts_as_deploy.role == "roles/iam.workloadIdentityUser"
    error_message = "the pool's only right on the deploy account is to impersonate it"
  }
}

run "pull_request_refs_cannot_be_allowed" {
  command = plan

  variables {
    github_ref = "refs/pull/1/merge"
  }

  expect_failures = [var.github_ref]
}

run "repository_must_be_owner_slash_name" {
  command = plan

  variables {
    github_repository = "example-org"
  }

  expect_failures = [var.github_repository]
}

run "only_the_service_signs_as_itself_and_only_the_deployer_acts_as_the_runtime_accounts" {
  command = apply

  assert {
    condition     = google_service_account_iam_member.run_signs_as_itself.role == "roles/iam.serviceAccountTokenCreator"
    error_message = "the service needs token creator on itself to sign URLs without a key"
  }
  assert {
    condition     = toset(keys(google_service_account_iam_member.deploy_acts_as)) == toset(["jobs", "run"])
    error_message = "the deploy account may act as the two runtime accounts and no other"
  }
  assert {
    condition     = alltrue([for m in google_service_account_iam_member.deploy_acts_as : m.role == "roles/iam.serviceAccountUser"])
    error_message = "acting as an account is roles/iam.serviceAccountUser"
  }
}

run "workloads_get_vertex_and_telemetry_roles_and_the_deployer_gets_no_project_role" {
  command = apply

  assert {
    condition = join(",", sort([for k, g in google_project_iam_member.workload : g.role])) == join(",", sort([
      "roles/aiplatform.user", "roles/aiplatform.user",
      "roles/logging.logWriter", "roles/logging.logWriter",
      "roles/monitoring.metricWriter", "roles/monitoring.metricWriter",
    ]))
    error_message = "run and jobs each get Vertex AI user, log writer and metric writer, nothing broader"
  }
  assert {
    condition     = alltrue([for k, g in google_project_iam_member.workload : !strcontains(g.member, "kiwi-deploy")])
    error_message = "the deploy account must hold no project-level role"
  }
}
