variable "project_id" {
  description = "The project that owns the network."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "region" {
  description = "The region of the subnets and of the Valkey service connection policy. Cloud Run, Cloud SQL and Valkey must all live here."
  type        = string
  default     = "asia-southeast1"
}

variable "network_name" {
  description = "Name of the VPC network."
  type        = string
  default     = "kiwi"
}

variable "run_subnet_cidr" {
  description = <<-EOT
    The subnet Cloud Run uses for Direct VPC egress. Cloud Run needs a /26 or larger; the default
    /24 leaves room for the two revisions that overlap during a rollout plus the Jobs' tasks, each
    of which holds a block of addresses while it runs.
  EOT
  type        = string
  default     = "10.20.0.0/24"

  validation {
    condition     = can(cidrhost(var.run_subnet_cidr, 0)) && tonumber(split("/", var.run_subnet_cidr)[1]) <= 26
    error_message = "run_subnet_cidr must be an IPv4 CIDR block of /26 or larger (Direct VPC egress needs at least a /26)."
  }
}

variable "valkey_subnet_cidr" {
  description = "A subnet reserved for the Private Service Connect endpoint of Memorystore for Valkey. Nothing else is placed in it."
  type        = string
  default     = "10.20.1.0/28"

  validation {
    condition     = can(cidrhost(var.valkey_subnet_cidr, 0)) && tonumber(split("/", var.valkey_subnet_cidr)[1]) <= 29
    error_message = "valkey_subnet_cidr must be an IPv4 CIDR block of /29 or larger."
  }
}

variable "private_services_address" {
  description = "First address of the range that private services access (Cloud SQL) takes out of the VPC."
  type        = string
  default     = "10.20.8.0"
}

variable "private_services_prefix_length" {
  description = "Prefix length of the private services access range."
  type        = number
  default     = 24

  validation {
    condition     = var.private_services_prefix_length >= 16 && var.private_services_prefix_length <= 24
    error_message = "private_services_prefix_length must be between 16 and 24."
  }
}

variable "flow_log_sampling" {
  description = "Share of the packets of the Cloud Run subnet recorded in VPC flow logs (0 to 1). The Logging free tier covers this volume."
  type        = number
  default     = 0.1

  validation {
    condition     = var.flow_log_sampling > 0 && var.flow_log_sampling <= 1
    error_message = "flow_log_sampling must be greater than 0 and at most 1."
  }
}
