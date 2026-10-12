variable "project_id" {
  description = "The project the budget watches."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a Google Cloud project id (6-30 characters: lowercase letters, digits and hyphens)."
  }
}

variable "billing_account" {
  description = "The billing account id (XXXXXX-XXXXXX-XXXXXX) that pays for the project."
  type        = string

  validation {
    condition     = can(regex("^[0-9A-F]{6}-[0-9A-F]{6}-[0-9A-F]{6}$", var.billing_account))
    error_message = "billing_account must look like 0123AB-4567CD-89EF01."
  }
}

variable "display_name" {
  description = "Name of the budget in the console."
  type        = string
  default     = "K.I.W.I."
}

variable "amount" {
  description = <<-EOT
    The monthly amount, in whole currency units, that the alerts are measured against. The default
    is the costed pilot month of the chosen design (Cloud Run + Cloud SQL db-g1-small + Valkey nano,
    about 360 USD with the Singapore Gemini prices) plus about a third of headroom. A budget alerts;
    it does not stop spending.
  EOT
  type        = number
  default     = 500

  validation {
    condition     = var.amount > 0 && floor(var.amount) == var.amount
    error_message = "amount must be a positive whole number."
  }
}

variable "currency_code" {
  description = "The currency of amount. It must be the currency of the billing account."
  type        = string
  default     = "USD"
}

variable "threshold_percents" {
  description = "Alert when actual spend reaches each of these shares of the amount (0.5 is 50%). The forecast is also alerted at 100%."
  type        = list(number)
  default     = [0.5, 0.8, 1.0]

  validation {
    condition     = length(var.threshold_percents) > 0 && alltrue([for p in var.threshold_percents : p > 0])
    error_message = "threshold_percents needs at least one positive share."
  }
}

variable "notification_channel_ids" {
  description = "Cloud Monitoring notification channels (projects/<project>/notificationChannels/<id>) to alert. The billing account's administrators and users are always emailed too, so no address is needed in this repository."
  type        = list(string)
  default     = []
}
