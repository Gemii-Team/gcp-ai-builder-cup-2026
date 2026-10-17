variable "project_id" {
  description = "The Google Cloud project the APIs are enabled in."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "services" {
  description = <<-EOT
    The APIs K.I.W.I. needs. The default covers every resource in deploy/terraform/modules.
    Nothing outside this list is enabled by Terraform; add to it with extra_services.
  EOT
  type        = set(string)
  default = [
    "artifactregistry.googleapis.com",     # Docker repository for the app images
    "billingbudgets.googleapis.com",       # the optional billing budget
    "cloudresourcemanager.googleapis.com", # project lookups and project-level IAM
    "compute.googleapis.com",              # VPC, subnets, private services access
    "iam.googleapis.com",                  # service accounts, Workload Identity Federation
    "iamcredentials.googleapis.com",       # keyless signing of Cloud Storage URLs, WIF token exchange
    "logging.googleapis.com",
    "meet.googleapis.com",        # Google Meet REST API (the app calls it with the user's own token)
    "memorystore.googleapis.com", # Memorystore for Valkey
    "monitoring.googleapis.com",
    "networkconnectivity.googleapis.com", # service connection policy for Valkey (Private Service Connect)
    "run.googleapis.com",                 # Cloud Run service and jobs
    "secretmanager.googleapis.com",
    "serviceusage.googleapis.com",      # Terraform and gcloud enable and check APIs through it
    "servicenetworking.googleapis.com", # private services access for Cloud SQL
    "sqladmin.googleapis.com",
    "storage.googleapis.com",
    "sts.googleapis.com",        # Security Token Service: GitHub OIDC token exchange
    "aiplatform.googleapis.com", # Vertex AI (Gemini)
  ]
}

variable "extra_services" {
  description = "Further APIs to enable on top of services."
  type        = set(string)
  default     = []
}

variable "propagation_wait" {
  description = <<-EOT
    How long to wait after enabling APIs before the next unit runs. A new API can answer
    "not enabled" for a minute or two after the enable call returns; the wait keeps the
    first apply of the later units from failing on that.
  EOT
  type        = string
  default     = "60s"

  validation {
    condition     = can(regex("^[0-9]+(s|m)$", var.propagation_wait))
    error_message = "propagation_wait must be a duration such as 60s or 2m."
  }
}
