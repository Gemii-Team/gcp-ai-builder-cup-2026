# Unit 4: the Docker repository for the app images. Only the deploy identity may push.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//registry"
}

dependencies {
  paths = ["../apis"]
}

dependency "iam" {
  config_path  = "../iam"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    deploy_member = "serviceAccount:kiwi-deploy@kiwi-offline-check.iam.gserviceaccount.com"
  }
  # Mock outputs stand in only where nothing is planned or applied: a plan made from mock values
  # could be applied by mistake.
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

inputs = {
  project_id     = include.root.locals.project_id
  region         = include.root.locals.region
  writer_members = [dependency.iam.outputs.deploy_member]
  labels         = include.root.locals.labels
}
