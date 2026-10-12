output "bucket_name" {
  description = "The uploads bucket name."
  value       = google_storage_bucket.uploads.name
}

output "bucket_url" {
  description = "The bucket as gs://<name>, for KIWI_BLOB_URL."
  value       = "gs://${google_storage_bucket.uploads.name}"
}
