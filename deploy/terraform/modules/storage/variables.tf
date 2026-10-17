variable "project_id" {
  description = "The project that owns the bucket."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "location" {
  description = "The bucket location (a region keeps the objects next to the service and cheapest to read from it)."
  type        = string
  default     = "asia-southeast1"
}

variable "bucket_name" {
  description = "The bucket name. Null means <project id>-kiwi-uploads. Bucket names are global and cannot be reused for a while after deletion."
  type        = string
  default     = null
}

variable "cors_origins" {
  description = "The origins whose pages may PUT and GET objects through signed URLs: the app's public origin (from the public-url module), without a path or trailing slash."
  type        = list(string)

  validation {
    condition     = length(var.cors_origins) > 0 && alltrue([for o in var.cors_origins : can(regex("^https://[a-z0-9.-]+(:[0-9]+)?$", o))])
    error_message = "cors_origins must list at least one https origin: a scheme and a host, no path, no trailing slash, no wildcard."
  }
}

variable "cors_response_headers" {
  description = <<-EOT
    The request and response headers the browser is allowed to use on a signed-URL request.
    Cloud Storage answers a preflight with exactly this list, so a header the app's uploader sets
    that is missing here fails the upload in the browser, not on the server. App-side contract:
    the GCS adapter (architecture review F03, F24) must stay within it.
  EOT
  type        = list(string)
  default     = ["Content-Type", "Content-MD5", "x-goog-content-length-range", "x-goog-content-sha256", "x-goog-if-generation-match"]
}

variable "abandoned_uploads_prefix" {
  description = <<-EOT
    The object name prefix of uploads that have not been accepted yet. App-side contract (F03,
    F26): the GCS adapter must move an accepted object out of this prefix, because the lifecycle
    rule deletes everything under it after abandoned_uploads_age_days, accepted or not.
  EOT
  type        = string
  default     = "uploads/"
}

variable "abandoned_uploads_age_days" {
  description = "Age in days after which an object under abandoned_uploads_prefix is deleted. A signed upload URL lives for minutes, so a few days is generous."
  type        = number
  default     = 7

  validation {
    condition     = var.abandoned_uploads_age_days >= 1
    error_message = "abandoned_uploads_age_days must be at least 1."
  }
}

variable "object_admin_members" {
  description = "IAM members (serviceAccount:...) that may create, read and delete objects in this bucket, and no other bucket: the Cloud Run service and the Jobs."
  type        = set(string)
  default     = []
}

variable "force_destroy" {
  description = "Whether destroying the bucket deletes the objects in it. Off: uploads are users' documents and recordings."
  type        = bool
  default     = false
}

variable "labels" {
  description = "Labels of the bucket."
  type        = map(string)
  default     = {}
}
