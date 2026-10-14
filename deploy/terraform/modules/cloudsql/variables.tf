variable "project_id" {
  description = "The project that owns the instance."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "region" {
  description = "The region of the instance (and of the DSN secrets)."
  type        = string
  default     = "asia-southeast1"
}

variable "zone" {
  description = "The zone of the instance. Null means the first zone of the region. Valkey is pinned to the same zone to keep the traffic between them off the cross-zone price."
  type        = string
  default     = null
}

variable "network_id" {
  description = "The VPC network the instance gets its private address in (from the network module)."
  type        = string
}

variable "private_services_range_name" {
  description = "The reserved private services access range the address is allocated from (from the network module)."
  type        = string
}

variable "instance_name" {
  description = "Name of the Cloud SQL instance. A deleted instance's name cannot be reused for about a week."
  type        = string
  default     = "kiwi-pg"
}

variable "database_name" {
  description = "The application database."
  type        = string
  default     = "kiwi"
}

variable "database_version" {
  description = "The PostgreSQL major version (the migrations need pgvector, which Cloud SQL ships with 16 and later)."
  type        = string
  default     = "POSTGRES_17"
}

variable "tier" {
  description = <<-EOT
    The machine tier. The default, db-g1-small (shared core, 1.7 GB), is what the full profile
    uses: with the app's built-in pools (20 + 16 per process) it needs 36 connections per
    instance and db-f1-micro starts at 25. The minimal profile passes db-f1-micro (0.614 GB)
    instead, with pools of 6 + 6 and max_connections raised to 50: the project owner's decision of
    2026-10-14, which accepts a memory risk that is reasoned, not measured. The research, the
    connection math, the signal to watch and the one-line way back (db-g1-small) are written next
    to the flag in main.tf. db-custom-1-3840 is the step above db-g1-small if memory ever becomes
    the limit.
  EOT
  type        = string
  default     = "db-g1-small"

  validation {
    condition     = can(regex("^db-(f1-micro|g1-small|custom-[0-9]+-[0-9]+|perf-optimized-N-[0-9]+)$", var.tier))
    error_message = "tier must be db-f1-micro, db-g1-small, db-custom-<vCPU>-<MB> or db-perf-optimized-N-<vCPU>."
  }
}

variable "edition" {
  description = <<-EOT
    ENTERPRISE or ENTERPRISE_PLUS. Always set: PostgreSQL 16 and later default to Enterprise Plus
    when it is omitted, which rejects every shared-core and db-custom tier at create time.
  EOT
  type        = string
  default     = "ENTERPRISE"

  validation {
    condition     = contains(["ENTERPRISE", "ENTERPRISE_PLUS"], var.edition)
    error_message = "edition must be ENTERPRISE or ENTERPRISE_PLUS."
  }
}

variable "max_connections" {
  description = <<-EOT
    The max_connections flag, or null to keep the tier's default (25 for db-f1-micro, 50 for
    db-g1-small, 100 from 3.75 GB). The default of 100 is explained next to the flag in main.tf.
    Changing it restarts the instance.
  EOT
  type        = number
  default     = 100
  nullable    = true

  validation {
    condition     = var.max_connections == null || var.max_connections >= 14
    error_message = "max_connections must be at least 14 (the lowest value Cloud SQL accepts) or null."
  }
}

variable "disk_size_gb" {
  description = "Initial SSD size in GB (10 is the minimum). It grows by itself up to disk_autoresize_limit_gb and never shrinks."
  type        = number
  default     = 20

  validation {
    condition     = var.disk_size_gb >= 10
    error_message = "disk_size_gb must be at least 10."
  }
}

variable "disk_autoresize_limit_gb" {
  description = "The size the disk may grow to by itself. 0 means no limit; the default caps a runaway table (a cost, not a safety, control)."
  type        = number
  default     = 100
}

variable "backup_start_time" {
  description = "Start of the daily backup window, HH:MM in UTC (the default is 01:00 in Thailand)."
  type        = string
  default     = "18:00"

  validation {
    condition     = can(regex("^([01][0-9]|2[0-3]):[0-5][0-9]$", var.backup_start_time))
    error_message = "backup_start_time must be HH:MM in 24-hour UTC."
  }
}

variable "backup_retained_count" {
  description = "How many automated backups to keep."
  type        = number
  default     = 7

  validation {
    condition     = var.backup_retained_count >= 1
    error_message = "backup_retained_count must be at least 1."
  }
}

variable "point_in_time_recovery" {
  description = "Whether write-ahead logs are kept so the database can be restored to a moment, not only to a nightly backup."
  type        = bool
  default     = true
}

variable "deletion_protection" {
  description = "Protects the instance from deletion both in Terraform and in Google Cloud (API, gcloud, console). Turn it off, apply, and only then destroy."
  type        = bool
  default     = true
}

variable "credentials_version" {
  description = <<-EOT
    The version of the generated database credentials. Raise it to rotate every password at once
    (the write-only arguments are sent again only when it changes). Order of a rotation: apply
    this module, run the kiwi-migrate Job (its bootstrap resets the kiwi_app, kiwi_system and
    kiwi_ro passwords to the new ones), then start a new Cloud Run revision so it reads the
    new secret versions.

    Raise it as well after an apply of this unit that failed half-way: the generated passwords
    exist only while one apply runs, so a user created in a failed run and a secret written in the
    retry would hold different passwords, and only a new version writes both again.
  EOT
  type        = number
  default     = 1
}

variable "runtime_members" {
  description = "IAM members (serviceAccount:...) that read the kiwi_app and kiwi_system connection strings: the Cloud Run service and the Jobs."
  type        = set(string)
  default     = []
}

variable "bootstrap_members" {
  description = "IAM members that also read the table owner's and the read-only role's connection strings: the Job that migrates and bootstraps the roles, and nothing else."
  type        = set(string)
  default     = []
}

variable "labels" {
  description = "Labels of the instance and the secrets."
  type        = map(string)
  default     = {}
}
