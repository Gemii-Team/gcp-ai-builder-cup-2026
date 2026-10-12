output "host" {
  description = "The private address of the primary endpoint."
  value       = one(local.primary).ip_address
}

output "port" {
  description = "The port of the primary endpoint."
  value       = one(local.primary).port
}

output "url" {
  description = "The connection URL for KIWI_VALKEY_URL. It holds no credential: authentication is off and the address is private."
  value       = "redis://${one(local.primary).ip_address}:${one(local.primary).port}"
}

output "instance_id" {
  description = "The Memorystore instance id."
  value       = google_memorystore_instance.this.instance_id
}
