# Source guard used by the module tests (terraform test): scans every .tf file of deploy/terraform
# for constructs this stack must never contain, so a plausible edit (a service account key, a
# plaintext password argument) fails a test instead of reaching an apply.
variable "root" {
  description = "The directory to scan. Null means deploy/terraform."
  type        = string
  default     = null
}

locals {
  root = coalesce(var.root, "${path.module}/../..")

  files = [
    for f in fileset(local.root, "**/*.tf") : f
    if !strcontains(f, ".terraform/") && !strcontains(f, ".terragrunt-cache/")
  ]

  # name => regular expression matched against the text of each file
  forbidden = {
    # A service account key is a long-lived credential: everything here authenticates by identity.
    service_account_key = "resource\\s+\"google_service_account_key\""
    # Secret values go through write-only arguments (password_wo, secret_data_wo) so they never reach state.
    plaintext_password    = "(?m)^\\s*password\\s*="
    plaintext_secret_data = "(?m)^\\s*secret_data\\s*="
    # Nothing in this stack accepts a connection from the internet except Cloud Run's front end.
    open_ingress_firewall = "source_ranges\\s*=\\s*\\[[^\\]]*\"0\\.0\\.0\\.0/0\""
    # Cloud SQL is private IP only.
    cloud_sql_public_ip = "ipv4_enabled\\s*=\\s*true"
  }

  offenders = {
    for name, pattern in local.forbidden : name => [
      for f in local.files : f if can(regex(pattern, file("${local.root}/${f}")))
    ]
  }
}

output "offenders" {
  description = "Rule name to the files that break it. Every list must be empty."
  value       = local.offenders
}

output "files_scanned" {
  description = "How many files were scanned (a guard that scans nothing proves nothing)."
  value       = length(local.files)
}
