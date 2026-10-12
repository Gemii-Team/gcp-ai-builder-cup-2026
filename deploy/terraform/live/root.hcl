# Root Terragrunt configuration for K.I.W.I.: one remote state per unit in one GCS bucket, one
# generated provider configuration, and the environment values every unit reads. A unit is a
# directory next to this file with a terragrunt.hcl; each runs one module of ../modules.
#
# Everything environment-specific comes from env.hcl (git-ignored; env.hcl.example lists the
# names). Nothing in the committed HCL names a project, a bucket, an account or a domain.
#
# Terragrunt never creates the state bucket here: the bucket is created by hand once (see the
# README) and the backend is not bootstrapped, because --backend-bootstrap is never passed.
#
# Apply order (each unit: plan to a file, review, apply exactly that file, delete it):
#   apis -> iam, network, public-url -> registry, storage, cloudsql, valkey, secrets -> cloudrun
#   budget, if a billing account is set, depends only on apis and can go any time after it.
# Dependencies are dependency blocks (outputs from the other unit's state) with mock outputs that
# stand in only for init, validate and providers, never for plan or apply.
#
# Runtime seam. Only the cloudrun unit (module cloudrun-app) knows the runtime. apis, iam, network,
# public-url, registry, storage, cloudsql, valkey, secrets and budget serve any runtime. To run on
# GKE instead, add a module for it (cluster, node pools or Autopilot, a Gateway and Kubernetes
# service accounts bound to kiwi-run and kiwi-jobs with roles/iam.workloadIdentityUser) and a unit
# beside cloudrun that depends on the same units, then drop cloudrun from the stack. Three things
# assume Cloud Run today and need that module's attention: the network's "run" subnet (a cluster
# brings its own node and pod ranges), the public-url module's default host (a run.app one; GKE
# needs custom_domain), and the identities' member strings (federated principals cannot sign
# URLs, so Workload Identity must stay in linked mode).
locals {
  # KIWI_IAC_ENV_FILE points the checks at env.check.hcl (fake values) so they need no real
  # project; a person applying never sets it.
  env_file = get_env("KIWI_IAC_ENV_FILE", "${get_parent_terragrunt_dir()}/env.hcl")
  env      = read_terragrunt_config(local.env_file).locals

  # Required.
  project_id        = local.env.project_id
  state_bucket      = local.env.state_bucket
  github_repository = local.env.github_repository

  # Optional: an empty value means "use the default".
  region          = coalesce(try(local.env.region, ""), "asia-southeast1")
  zone            = coalesce(try(local.env.zone, ""), "${local.region}-a")
  custom_domain   = try(local.env.custom_domain, "")
  billing_account = try(local.env.billing_account, "")

  # Vertex AI. Not decided yet: Singapore (gemini-3.5-flash, in-region processing) or the global
  # endpoint. Changing the location is changing these values, in env.hcl, and nothing else.
  vertex_location           = coalesce(try(local.env.vertex_location, ""), "asia-southeast1")
  vertex_embedding_location = coalesce(try(local.env.vertex_embedding_location, ""), "asia-southeast1")
  vertex_model_flash        = coalesce(try(local.env.vertex_model_flash, ""), "gemini-3.5-flash")
  vertex_model_pro          = try(local.env.vertex_model_pro, "") != "" ? local.env.vertex_model_pro : null
  vertex_embedding_model    = coalesce(try(local.env.vertex_embedding_model, ""), "gemini-embedding-001")

  # Empty until CI has built the images: the service then starts from Google's public hello image
  # (see modules/cloudrun-app). After the first deploy CI owns the images, not Terraform.
  web_image  = try(local.env.web_image, "") != "" ? local.env.web_image : null
  kiwi_image = try(local.env.kiwi_image, "") != "" ? local.env.kiwi_image : null

  # The Cloud Run service name is part of the default URL, so the public-url module and the
  # cloudrun unit must agree on it: it is defined once, here.
  service_name = "kiwi-app"

  labels = {
    app        = "kiwi"
    managed-by = "terraform"
  }

  modules_dir = "${get_parent_terragrunt_dir()}/../modules"

  # Offline checks (task iac:check) set this so no dependency's real outputs are read from the
  # remote state, which would reach Google Cloud Storage: the mock outputs stand in, and the
  # backend is configured but never initialised.
  offline = get_env("KIWI_IAC_OFFLINE", "false") == "true"
}

# The terraform binary: Terraform itself, not OpenTofu (Terragrunt defaults to tofu).
terraform_binary = "terraform"

# The module tests (terraform test, run on the modules directly) reference a source guard outside
# the copied tree, and a copy that is only planned and applied has no use for them.
terraform {
  exclude_from_copy = ["**/tests/**", "**/.terraform/**"]
}

remote_state {
  backend      = "gcs"
  disable_init = local.offline

  generate = {
    path      = "backend.tf"
    if_exists = "overwrite_terragrunt"
  }

  config = {
    bucket = local.state_bucket
    # One state per unit: kiwi/<unit>/default.tfstate.
    prefix = "kiwi/${path_relative_to_include()}"
  }
}

# Credentials come from the caller's environment (gcloud auth application-default login as a
# project owner); no key file is ever configured. user_project_override sends quota and billing
# of API calls to the project, which the billing budget API requires of user credentials.
generate "provider" {
  path      = "provider.tf"
  if_exists = "overwrite_terragrunt"
  contents  = <<-EOF
    provider "google" {
      project               = "${local.project_id}"
      region                = "${local.region}"
      user_project_override = true
      billing_project       = "${local.project_id}"
    }
  EOF
}
