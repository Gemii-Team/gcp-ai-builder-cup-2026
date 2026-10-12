output "secret_ids" {
  description = "Environment variable name to secret id, for every secret of this module: what a Cloud Run secret reference takes. No value is in any output."
  value = merge(
    { for name, m in module.generated : name => m.secret_id },
    { for name, m in module.external : name => m.secret_id },
  )
}

output "external_secret_ids" {
  description = "Environment variable name to secret id of the secrets whose value is added by hand."
  value       = { for name, m in module.external : name => m.secret_id }
}
