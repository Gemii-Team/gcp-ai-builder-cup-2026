output "budget_name" {
  description = "The full resource name of the budget (billingAccounts/<id>/budgets/<id>)."
  value       = google_billing_budget.this.name
}
