# Unit 3: the VPC, the Cloud Run subnet and private services access for Cloud SQL; in the full
# profile also the Private Service Connect subnet and the service connection policy for Valkey.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//network"
}

dependencies {
  paths = ["../apis"]
}

inputs = {
  project_id = include.root.locals.project_id
  region     = include.root.locals.region

  # Only the full profile has a Valkey that needs its own subnet and connection policy.
  valkey_enabled = !include.root.locals.minimal
}
