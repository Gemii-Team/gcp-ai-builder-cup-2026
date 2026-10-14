# Unit 5: PostgreSQL on Cloud SQL (private IP only), the application database, the table owner as
# the one Cloud SQL user, and the generated connection strings as Secret Manager secrets. The
# full profile takes the module's defaults (db-g1-small, max_connections 100); the minimal one
# passes db-f1-micro with max_connections 50 for its smaller pools, the project owner's decision
# of 2026-10-14 (see root.hcl and modules/cloudsql/main.tf for the risk and the way back).
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

inputs = merge(
  {
    project_id                  = include.root.locals.project_id
    region                      = include.root.locals.region
    zone                        = include.root.locals.zone
    network_id                  = dependency.network.outputs.network_id
    private_services_range_name = dependency.network.outputs.private_services_range_name
    runtime_members             = [dependency.iam.outputs.run_member, dependency.iam.outputs.jobs_member]
    bootstrap_members           = [dependency.iam.outputs.jobs_member]
    labels                      = include.root.locals.labels
  },
  # The minimal profile's tier and connection budget, written out so the instance does not depend
  # on the module's defaults staying what they are today. The full profile passes neither.
  include.root.locals.minimal ? {
    tier            = include.root.locals.minimal_sql_tier
    max_connections = include.root.locals.minimal_sql_max_connections
  } : {},
)
