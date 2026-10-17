output "project_number" {
  description = "The numeric project number (the deterministic run.app URL embeds it)."
  value       = data.google_project.this.number
}

output "enabled_services" {
  description = "The APIs this module enabled, once the propagation wait has passed."
  value       = sort(tolist(local.services))
  depends_on  = [time_sleep.propagation]
}
