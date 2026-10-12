variable "project_id" {
  description = "The project that owns the secret."
  type        = string
}

variable "secret_id" {
  description = "The Secret Manager secret id."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_-]{1,255}$", var.secret_id))
    error_message = "secret_id may hold letters, digits, hyphens and underscores only (at most 255 characters)."
  }
}

variable "region" {
  description = "The one region the secret is replicated to, so its value never leaves the region of the stack."
  type        = string
}

variable "mode" {
  description = <<-EOT
    generated: Terraform sets the value, from value_wo, and it never reaches the state.
    external: the value is added out of band (gcloud or the console); Terraform only creates
    the secret and a placeholder version, because Cloud Run refuses to start a revision whose
    secret has no version, and the first apply must succeed before anyone has added one.
  EOT
  type        = string
  default     = "generated"

  validation {
    condition     = contains(["generated", "external"], var.mode)
    error_message = "mode must be generated or external."
  }
}

variable "value_wo" {
  description = "The value of a generated secret. Ephemeral: it is passed to a write-only argument and is in neither plan nor state."
  type        = string
  default     = null
  ephemeral   = true
  sensitive   = true
}

variable "value_version" {
  description = "Increase it to write value_wo again (a write-only argument is only sent when its version changes)."
  type        = number
  default     = 1
}

variable "placeholder" {
  description = "The text of the placeholder version of an external secret. It is not a secret: the app fails visibly on it, which is the intent until the real value is added."
  type        = string
  default     = "replace-me-out-of-band"
}

variable "accessor_members" {
  description = "IAM members (serviceAccount:...) that may read this secret's versions. The grant is on this secret alone."
  type        = set(string)
  default     = []
}

variable "labels" {
  description = "Labels of the secret."
  type        = map(string)
  default     = {}
}

variable "deletion_protection" {
  description = "Whether Terraform refuses to destroy the secret. Losing a generated key can make stored data unreadable."
  type        = bool
  default     = true
}
