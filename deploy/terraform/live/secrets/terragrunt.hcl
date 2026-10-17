# Unit 6: the application secrets: the Google OAuth client id and secret (values added by hand)
# and the two generated keys. The database connection strings are in the cloudsql unit.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//secrets"
}

dependencies {
  paths = ["../apis"]
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
  project_id       = include.root.locals.project_id
  region           = include.root.locals.region
  accessor_members = [dependency.iam.outputs.run_member, dependency.iam.outputs.jobs_member]
  labels           = include.root.locals.labels
}
