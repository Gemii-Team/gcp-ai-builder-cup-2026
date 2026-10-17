# One Secret Manager secret, the unit the rest of the stack is built from: one secret per
# application variable, a read grant per member on that secret only, and a value that never
# lands in Terraform state (write-only argument).
resource "google_secret_manager_secret" "this" {
  project   = var.project_id
  secret_id = var.secret_id
  labels    = var.labels

  deletion_protection = var.deletion_protection

  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }
}

resource "google_secret_manager_secret_version" "generated" {
  count = var.mode == "generated" ? 1 : 0

  secret                 = google_secret_manager_secret.this.id
  secret_data_wo         = var.value_wo
  secret_data_wo_version = var.value_version
}

resource "google_secret_manager_secret_version" "placeholder" {
  count = var.mode == "external" ? 1 : 0

  secret = google_secret_manager_secret.this.id
  # Written once and never changed: a version added later (gcloud secrets versions add)
  # becomes "latest" and this one stays behind, untouched by Terraform.
  secret_data_wo         = var.placeholder
  secret_data_wo_version = 1
}

resource "google_secret_manager_secret_iam_member" "accessor" {
  for_each = var.accessor_members

  project   = var.project_id
  secret_id = google_secret_manager_secret.this.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = each.value
}
