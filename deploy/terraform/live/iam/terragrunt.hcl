# Unit 2: the three identities (kiwi-run, kiwi-jobs, kiwi-deploy) and Workload Identity
# Federation for GitHub. Other units grant these identities access to their own resources.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//iam"
}

dependencies {
  paths = ["../apis"]
}

inputs = {
  project_id        = include.root.locals.project_id
  github_repository = include.root.locals.github_repository
}
