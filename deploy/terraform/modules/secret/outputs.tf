output "secret_id" {
  description = "The short secret id, which is what a Cloud Run secret reference takes."
  value       = google_secret_manager_secret.this.secret_id
  depends_on = [
    google_secret_manager_secret_version.generated,
    google_secret_manager_secret_version.placeholder,
    google_secret_manager_secret_iam_member.accessor,
  ]
}
