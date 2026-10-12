# Unit 1: the Google APIs the stack needs. Everything else depends on it.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//project-services"
}

inputs = {
  project_id = include.root.locals.project_id
}
