variable "project_id" {
  description = "The project the Cloud Run service lives in (its number is part of the default URL)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "service_name" {
  description = "The Cloud Run service name; it is the first label of the default URL."
  type        = string
  default     = "kiwi-app"

  validation {
    condition     = can(regex("^[a-z]([a-z0-9-]{0,40}[a-z0-9])?$", var.service_name))
    error_message = "service_name must be a Cloud Run service name (lowercase letters, digits and hyphens, at most 42 characters, starting with a letter)."
  }
}

variable "region" {
  description = "The Cloud Run region."
  type        = string
  default     = "asia-southeast1"
}

variable "custom_domain" {
  description = <<-EOT
    A host name that serves the app instead of the default URL, or empty. Changing it only changes
    the URL this module returns; mapping the name to the service (a Cloud Run domain mapping or a
    load balancer) is not part of this stack.
  EOT
  type        = string
  default     = ""

  validation {
    condition     = var.custom_domain == "" || can(regex("^([a-z0-9]([a-z0-9-]*[a-z0-9])?\\.)+[a-z]{2,}$", var.custom_domain))
    error_message = "custom_domain must be a bare lowercase host name: no scheme, no port, no path, no trailing slash. The app compares the browser's Origin with KIWI_PUBLIC_URL exactly."
  }
}
