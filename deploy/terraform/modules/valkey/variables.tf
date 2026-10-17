variable "project_id" {
  description = "The project that owns the instance."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "region" {
  description = "The region of the instance. It must be the region of the network's service connection policy."
  type        = string
  default     = "asia-southeast1"
}

variable "zone" {
  description = "The zone of the single node. Null means the first zone of the region; keep it equal to the Cloud SQL zone."
  type        = string
  default     = null
}

variable "network_id" {
  description = "The VPC network the Private Service Connect endpoint is created in (from the network module, whose service connection policy must exist first)."
  type        = string
}

variable "instance_id" {
  description = "Name of the Memorystore for Valkey instance."
  type        = string
  default     = "kiwi-valkey"
}

variable "node_type" {
  description = "SHARED_CORE_NANO (1.4 GB, no SLA, the smallest) or a larger node type."
  type        = string
  default     = "SHARED_CORE_NANO"
}

variable "engine_version" {
  description = "The Valkey engine version (for example VALKEY_8_0), or null for the version Memorystore currently offers."
  type        = string
  default     = null
}

variable "deletion_protection" {
  description = "Whether the instance refuses deletion. Sessions and tickets are lost with it, so the default is on."
  type        = bool
  default     = true
}

variable "labels" {
  description = "Labels of the instance."
  type        = map(string)
  default     = {}
}
