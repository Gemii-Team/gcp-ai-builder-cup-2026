# Three identities, none of them with a key (a service account key is a long-lived credential
# that can leak; everything here authenticates by what it runs as):
#
#   kiwi-run     the Cloud Run service. Reads its secrets, Vertex AI, the uploads bucket.
#   kiwi-jobs    the migrate and seed Jobs. The only identity that reads the table owner's
#                connection string, so it is the only one that can run DDL.
#   kiwi-deploy  GitHub Actions, through Workload Identity Federation: pushes images and
#                deploys the service and the Jobs. It cannot touch data.
#
# What each may do on a particular resource (a secret, the bucket, the registry, the service) is
# granted by the module that owns the resource, on that resource alone; this module only grants
# what is not tied to one resource: Vertex AI, logging, signing as itself, and deploying as the
# runtime identities.
locals {
  accounts = {
    run    = { id = var.service_account_ids.run, name = "K.I.W.I. Cloud Run service" }
    jobs   = { id = var.service_account_ids.jobs, name = "K.I.W.I. migrate and seed Jobs" }
    deploy = { id = var.service_account_ids.deploy, name = "K.I.W.I. GitHub Actions deploy" }
  }

  # The identities that run application code, and so call Vertex AI and write logs.
  workloads = toset(["run", "jobs"])

  workload_roles = [
    "roles/aiplatform.user", # Gemini and embeddings on Vertex AI
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
  ]
  workload_grants = {
    for pair in setproduct(local.workloads, local.workload_roles) :
    "${pair[0]}/${pair[1]}" => { account = pair[0], role = pair[1] }
  }

  repository_principal = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repository}"
}

resource "google_service_account" "this" {
  for_each = local.accounts

  project      = var.project_id
  account_id   = each.value.id
  display_name = each.value.name
}

resource "google_project_iam_member" "workload" {
  for_each = local.workload_grants

  project = var.project_id
  role    = each.value.role
  member  = google_service_account.this[each.value.account].member
}

# Keyless v4 signed URLs: the service signs with its own identity through the IAM Credentials
# API, which needs the right to create tokens for itself. Only the service signs URLs for the
# browser; the Jobs write objects through the API directly.
resource "google_service_account_iam_member" "run_signs_as_itself" {
  service_account_id = google_service_account.this["run"].name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = google_service_account.this["run"].member
}

# Deploying a revision or running a Job "as" an account needs the right to act as it. The deploy
# identity gets it on the two runtime identities, not project-wide.
resource "google_service_account_iam_member" "deploy_acts_as" {
  for_each = local.workloads

  service_account_id = google_service_account.this[each.key].name
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.this["deploy"].member
}

resource "google_iam_workload_identity_pool" "github" {
  project                   = var.project_id
  workload_identity_pool_id = var.workload_identity_pool_id
  display_name              = "GitHub Actions"
  description               = "Keyless access for the workflows of ${var.github_repository}"
}

resource "google_iam_workload_identity_pool_provider" "github" {
  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = var.workload_identity_provider_id
  display_name                       = "GitHub OIDC"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"
  }

  # The gate. A token that fails this condition is refused before any binding is looked at, so
  # no other repository of any organisation, and no other branch or pull request of this one,
  # can ever obtain a credential from this pool.
  attribute_condition = "assertion.repository == '${var.github_repository}' && assertion.ref == '${var.github_ref}'"

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

# The pool's identities act as the deploy account, nothing else.
resource "google_service_account_iam_member" "github_acts_as_deploy" {
  service_account_id = google_service_account.this["deploy"].name
  role               = "roles/iam.workloadIdentityUser"
  member             = local.repository_principal
}
