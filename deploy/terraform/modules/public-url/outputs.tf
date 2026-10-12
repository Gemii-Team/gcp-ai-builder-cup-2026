output "public_url" {
  description = "The origin of the app: https, a host, no path, no trailing slash. This is KIWI_PUBLIC_URL."
  value       = "https://${local.host}"
}

output "host" {
  description = "The host of public_url."
  value       = local.host
}

output "oauth_redirect_uri" {
  description = "The redirect URI to register in the Google OAuth client (the app builds it from KIWI_PUBLIC_URL)."
  value       = "https://${local.host}/api/auth/callback"
}

output "uses_custom_domain" {
  description = "Whether a custom domain, not the default Cloud Run URL, is the origin."
  value       = var.custom_domain != ""
}
