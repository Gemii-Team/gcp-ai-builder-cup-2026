output "instance_name" {
  description = "The Cloud SQL instance name."
  value       = google_sql_database_instance.this.name
}

output "connection_name" {
  description = "The instance connection name (project:region:instance), for the Auth Proxy, gcloud and the console."
  value       = google_sql_database_instance.this.connection_name
}

output "private_ip_address" {
  description = "The private address of the instance (it has no public one)."
  value       = google_sql_database_instance.this.private_ip_address
}

output "database_name" {
  description = "The application database."
  value       = google_sql_database.app.name
}

output "owner_secret_ids" {
  description = "Environment variable name to secret id of the table owner's connection string: for the migrate Job only."
  value       = { for name, m in module.dsn : name => m.secret_id if local.dsn_secrets[name].kind == "owner" }
}

output "runtime_secret_ids" {
  description = "Environment variable name to secret id of the kiwi_app and kiwi_system connection strings: for the service and the Jobs."
  value       = { for name, m in module.dsn : name => m.secret_id if local.dsn_secrets[name].kind == "runtime" }
}

output "readonly_secret_ids" {
  description = "Environment variable name to secret id of the kiwi_ro connection string: read only by the bootstrap step that creates the role."
  value       = { for name, m in module.dsn : name => m.secret_id if local.dsn_secrets[name].kind == "readonly" }
}
