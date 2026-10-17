# Unit 4: the uploads bucket: private, uniform access, CORS for the app origin, object admin for
# the service and the Jobs on this bucket only.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//storage"
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

dependency "public_url" {
  config_path  = "../public-url"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    public_url = "https://kiwi-app-000000000000.asia-southeast1.run.app"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

inputs = {
  project_id           = include.root.locals.project_id
  location             = include.root.locals.region
  cors_origins         = [dependency.public_url.outputs.public_url]
  object_admin_members = [dependency.iam.outputs.run_member, dependency.iam.outputs.jobs_member]
  labels               = include.root.locals.labels
}
