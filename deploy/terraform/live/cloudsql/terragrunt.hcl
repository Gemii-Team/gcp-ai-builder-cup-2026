# Unit 5: PostgreSQL on Cloud SQL (private IP only), the application database, the table owner as
# the one Cloud SQL user, and the generated connection strings as Secret Manager secrets.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//cloudsql"
}

dependencies {
  paths = ["../apis"]
}

dependency "network" {
  config_path  = "../network"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    network_id                  = "projects/kiwi-offline-check/global/networks/kiwi"
    private_services_range_name = "kiwi-private-services"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

dependency "iam" {
  config_path  = "../iam"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    run_member  = "serviceAccount:kiwi-run@kiwi-offline-check.iam.gserviceaccount.com"
    jobs_member = "serviceAccount:kiwi-jobs@kiwi-offline-check.iam.gserviceaccount.com"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

inputs = {
  project_id                  = include.root.locals.project_id
  region                      = include.root.locals.region
  zone                        = include.root.locals.zone
  network_id                  = dependency.network.outputs.network_id
  private_services_range_name = dependency.network.outputs.private_services_range_name
  runtime_members             = [dependency.iam.outputs.run_member, dependency.iam.outputs.jobs_member]
  bootstrap_members           = [dependency.iam.outputs.jobs_member]
  labels                      = include.root.locals.labels
}
