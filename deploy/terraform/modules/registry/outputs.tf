output "repository_path" {
  description = "The registry path images are pushed under: <region>-docker.pkg.dev/<project>/<repository>. Append /kiwi:<tag>: the one image of the minimal profile, which also holds the web app. The full profile has a second one, /web:<tag>, the Caddy ingress."
  value       = "${google_artifact_registry_repository.this.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.this.repository_id}"
}

output "repository_id" {
  description = "The repository id."
  value       = google_artifact_registry_repository.this.repository_id
}
