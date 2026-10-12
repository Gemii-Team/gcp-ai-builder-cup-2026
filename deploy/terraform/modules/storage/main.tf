# Uploads (documents, recordings) live in Cloud Storage, not on the instance: a Cloud Run
# instance's disk is memory and is lost with it, and a request through the app is capped at 32 MiB
# while recordings reach 200 MiB. The browser PUTs and GETs objects directly through short-lived
# signed URLs the app creates with its service account's own identity (no key file).
# Google-managed encryption is deliberate: a customer-managed key adds a Cloud KMS key to run and
# pay for and a way to make every upload unreadable by losing it, for a demo workspace's documents.
# The bucket is private, uniform-access and encrypted at rest by Google.
#trivy:ignore:AVD-GCP-0066
resource "google_storage_bucket" "uploads" {
  project  = var.project_id
  name     = coalesce(var.bucket_name, "${var.project_id}-kiwi-uploads")
  location = var.location
  labels   = var.labels

  storage_class = "STANDARD"
  force_destroy = var.force_destroy

  # No ACLs, no allUsers, no allAuthenticatedUsers, ever: access is IAM on this bucket, and
  # a signed URL, which is IAM acting through a service account.
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  # Browser PUT (upload) and GET/HEAD (download) of signed URLs, from the app's own origin only.
  cors {
    origin          = var.cors_origins
    method          = ["GET", "HEAD", "PUT"]
    response_header = var.cors_response_headers
    max_age_seconds = 3600
  }

  # Uploads that were started and never accepted would otherwise stay for ever (F26).
  lifecycle_rule {
    condition {
      age            = var.abandoned_uploads_age_days
      matches_prefix = [var.abandoned_uploads_prefix]
      with_state     = "LIVE"
    }
    action {
      type = "Delete"
    }
  }
}

resource "google_storage_bucket_iam_member" "object_admin" {
  for_each = var.object_admin_members

  bucket = google_storage_bucket.uploads.name
  role   = "roles/storage.objectAdmin"
  member = each.value
}
