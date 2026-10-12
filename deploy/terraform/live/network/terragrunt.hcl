# Unit 3: the VPC, the Cloud Run subnet, private services access for Cloud SQL and the service
# connection policy for Valkey.
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
}
