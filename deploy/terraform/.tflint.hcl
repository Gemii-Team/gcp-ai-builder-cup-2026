# tflint over every module: the Terraform language rules (recommended preset) and the Google
# provider rules (invalid machine types, regions, enum values that terraform validate accepts).
# `tflint --init` downloads the google ruleset; the checks run with no cloud credentials.
config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

plugin "google" {
  enabled = true
  version = "0.40.0"
  source  = "github.com/terraform-linters/tflint-ruleset-google"
}
