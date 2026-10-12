# Unit 5: Memorystore for Valkey: one cluster-disabled node, private endpoint.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//valkey"
}

dependencies {
  paths = ["../apis"]
}

# The network unit also creates the service connection policy the instance's endpoint needs, so
# depending on it orders the policy before the instance.
dependency "network" {
  config_path  = "../network"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    network_id = "projects/kiwi-offline-check/global/networks/kiwi"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

inputs = {
  project_id = include.root.locals.project_id
  region     = include.root.locals.region
  zone       = include.root.locals.zone
  network_id = dependency.network.outputs.network_id
  labels     = include.root.locals.labels
}
