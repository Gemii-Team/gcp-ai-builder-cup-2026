output "repository_path" {
  description = "The registry path images are pushed under: <region>-docker.pkg.dev/<project>/<repository>. Append /web:<tag> or /kiwi:<tag>."
  value       = "${google_artifact_registry_repository.this.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.this.repository_id}"
}

output "repository_id" {
  description = "The repository id."
  value       = google_artifact_registry_repository.this.repository_id
}
