# Unit 3: the app's origin (the deterministic run.app URL, or the custom domain), computed once.
# Its outputs are the KIWI_PUBLIC_URL of the service, the CORS origin of the bucket, and the
# redirect URI to register in the Google OAuth client.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//public-url"
}

dependencies {
  paths = ["../apis"]
}

inputs = {
  project_id    = include.root.locals.project_id
  region        = include.root.locals.region
  service_name  = include.root.locals.service_name
  custom_domain = include.root.locals.custom_domain
}
