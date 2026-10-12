# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

variables {
  project_id      = "kiwi-test-123"
  billing_account = "0123AB-4567CD-89EF01"
}

run "alerts_on_actual_spend_and_on_the_forecast" {
  command = apply

  assert {
    condition     = length([for r in google_billing_budget.this.threshold_rules : r if r.spend_basis == "CURRENT_SPEND"]) == 3
    error_message = "the default alerts actual spend at 50%, 80% and 100%"
  }
  assert {
    condition     = length([for r in google_billing_budget.this.threshold_rules : r if r.spend_basis == "FORECASTED_SPEND" && r.threshold_percent == 1]) == 1
    error_message = "the forecast must be alerted at 100%"
  }
}

run "budget_watches_this_project_only" {
  command = apply

  assert {
    condition     = one(google_billing_budget.this.budget_filter).projects == toset(["projects/123456789012"])
    error_message = "the budget must be filtered to this project's number"
  }
}

run "billing_administrators_are_emailed_without_an_address_in_the_repository" {
  command = apply

  assert {
    condition     = one(google_billing_budget.this.all_updates_rule).disable_default_iam_recipients == false
    error_message = "the default IAM recipients (billing account administrators and users) must be notified"
  }
}

run "billing_account_must_look_like_one" {
  command = plan

  variables {
    billing_account = "not-an-account"
  }

  expect_failures = [var.billing_account]
}
