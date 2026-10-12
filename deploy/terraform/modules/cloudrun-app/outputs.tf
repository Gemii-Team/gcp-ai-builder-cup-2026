output "service_name" {
  description = "The Cloud Run service name."
  value       = google_cloud_run_v2_service.this.name
}

output "public_url" {
  description = "The origin the app was configured with (KIWI_PUBLIC_URL)."
  value       = var.public_url
}

output "service_uri" {
  description = "The URL Cloud Run reports for the service. With the default service name it should equal public_url; if the two differ, the OAuth redirect URI and the Origin check are wrong."
  value       = google_cloud_run_v2_service.this.uri
}

output "migrate_job_name" {
  description = "The migrate Job."
  value       = google_cloud_run_v2_job.migrate.name
}

output "seed_job_name" {
  description = "The seed Job."
  value       = google_cloud_run_v2_job.seed.name
}
