variable "project_id" {
  description = "The project that owns the repository."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "region" {
  description = "The region of the repository (the same as Cloud Run, so image pulls stay in the region)."
  type        = string
  default     = "asia-southeast1"
}

variable "repository_id" {
  description = "Name of the Docker repository. The images are <region>-docker.pkg.dev/<project>/<repository_id>/web and .../kiwi."
  type        = string
  default     = "kiwi"
}

variable "keep_recent_versions" {
  description = "How many of the most recent versions of each image to keep when cleaning up."
  type        = number
  default     = 10

  validation {
    condition     = var.keep_recent_versions >= 2
    error_message = "keep_recent_versions must be at least 2 (the running image and the one to roll back to)."
  }
}

variable "untagged_ttl_days" {
  description = "Days after which an image without a tag is deleted."
  type        = number
  default     = 7
}

variable "writer_members" {
  description = "IAM members (serviceAccount:...) that may push images to this repository, and to no other: the GitHub Actions deploy account."
  type        = set(string)
  default     = []
}

variable "labels" {
  description = "Labels of the repository."
  type        = map(string)
  default     = {}
}
