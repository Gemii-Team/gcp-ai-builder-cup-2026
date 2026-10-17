# Spend is otherwise uncapped on a public URL that calls a metered model (architecture review F19).
# A budget cannot stop it, but it makes a runaway visible before the invoice does.
data "google_project" "this" {
  project_id = var.project_id
}

resource "google_billing_budget" "this" {
  billing_account = var.billing_account
  display_name    = var.display_name

  budget_filter {
    projects = ["projects/${data.google_project.this.number}"]
  }

  amount {
    specified_amount {
      currency_code = var.currency_code
      units         = tostring(var.amount)
    }
  }

  dynamic "threshold_rules" {
    for_each = var.threshold_percents
    content {
      threshold_percent = threshold_rules.value
      spend_basis       = "CURRENT_SPEND"
    }
  }

  threshold_rules {
    threshold_percent = 1.0
    spend_basis       = "FORECASTED_SPEND"
  }

  all_updates_rule {
    monitoring_notification_channels = var.notification_channel_ids

    # Billing account administrators and users are emailed when a threshold is crossed.
    disable_default_iam_recipients = false
  }
}
