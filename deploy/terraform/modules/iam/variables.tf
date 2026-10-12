variable "project_id" {
  description = "The project the identities live in."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "github_repository" {
  description = "The GitHub repository (owner/name) whose workflows may act as the deploy account."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must be owner/name."
  }
}

variable "github_ref" {
  description = "The one git ref whose workflow runs may act as the deploy account. Pull-request runs (refs/pull/...) are therefore refused: nothing a branch or a fork changes can deploy."
  type        = string
  default     = "refs/heads/main"

  validation {
    condition     = can(regex("^refs/(heads|tags)/[A-Za-z0-9._/-]+$", var.github_ref))
    error_message = "github_ref must be a full ref such as refs/heads/main."
  }
}

variable "service_account_ids" {
  description = "The account ids of the three identities."
  type = object({
    run    = optional(string, "kiwi-run")
    jobs   = optional(string, "kiwi-jobs")
    deploy = optional(string, "kiwi-deploy")
  })
  default = {}
}

variable "workload_identity_pool_id" {
  description = "Id of the Workload Identity pool for GitHub."
  type        = string
  default     = "kiwi-github"
}

variable "workload_identity_provider_id" {
  description = "Id of the OIDC provider of GitHub Actions inside the pool."
  type        = string
  default     = "github"
}
