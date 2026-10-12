output "network_id" {
  description = "The id of the VPC network (projects/<project>/global/networks/<name>). Cloud SQL and Valkey attach to it; the value is only known once private services access exists."
  value       = google_compute_network.this.id
  depends_on  = [google_service_networking_connection.private_services]
}

output "network_name" {
  description = "The name of the VPC network."
  value       = google_compute_network.this.name
}

output "run_subnet_name" {
  description = "The subnet for Cloud Run Direct VPC egress."
  value       = google_compute_subnetwork.run.name
}

output "private_services_range_name" {
  description = "The name of the reserved range Cloud SQL allocates its private address from."
  value       = google_compute_global_address.private_services.name
}

output "valkey_connection_policy_id" {
  description = "The service connection policy that lets Valkey create its endpoint; the instance must be created after it."
  value       = google_network_connectivity_service_connection_policy.valkey.id
}
