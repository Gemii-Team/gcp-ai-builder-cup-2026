# Optional unit: a billing budget with alerts. Created only when env.hcl names a billing account;
# otherwise Terragrunt leaves the unit out of every run (no empty stub).
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

exclude {
  if      = include.root.locals.billing_account == ""
  actions = ["all"]
}

terraform {
  source = "${include.root.locals.modules_dir}//budget"
}

dependencies {
  paths = ["../apis"]
}

inputs = {
  project_id      = include.root.locals.project_id
  billing_account = include.root.locals.billing_account
}
