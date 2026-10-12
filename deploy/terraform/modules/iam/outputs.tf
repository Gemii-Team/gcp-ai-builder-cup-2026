output "run_service_account_email" {
  description = "Email of the identity of the Cloud Run service."
  value       = google_service_account.this["run"].email
}

output "jobs_service_account_email" {
  description = "Email of the identity of the migrate and seed Jobs."
  value       = google_service_account.this["jobs"].email
}

output "deploy_service_account_email" {
  description = "Email of the identity GitHub Actions deploys as (the service_account input of google-github-actions/auth)."
  value       = google_service_account.this["deploy"].email
}

output "run_member" {
  description = "IAM member string (serviceAccount:...) of the Cloud Run service identity, for grants on resources."
  value       = google_service_account.this["run"].member
}

output "jobs_member" {
  description = "IAM member string of the Jobs identity."
  value       = google_service_account.this["jobs"].member
}

output "deploy_member" {
  description = "IAM member string of the deploy identity."
  value       = google_service_account.this["deploy"].member
}

output "workload_identity_provider" {
  description = "Full name of the Workload Identity provider (the workload_identity_provider input of google-github-actions/auth)."
  value       = google_iam_workload_identity_pool_provider.github.name
}
