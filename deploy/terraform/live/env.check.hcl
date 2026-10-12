# Fake values for the offline checks (task iac:check, the iac CI job): they let every unit render
# and validate with no real project. Nothing here exists in Google Cloud; do not apply with it.
locals {
  project_id        = "kiwi-offline-check"
  state_bucket      = "kiwi-offline-check-tfstate"
  github_repository = "example-org/example-repo"
  billing_account   = "000000-000000-000000"
}
