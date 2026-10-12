# The application secrets that are not database credentials (those belong to the cloudsql module,
# which generates them next to the passwords they carry). One Secret Manager secret per
# environment variable of the binary, named after it (KIWI_OAUTH_CLIENT_ID -> kiwi-oauth-client-id).

# 32 random bytes, base64: the format the app decodes (backend/internal/platform/config).
ephemeral "random_bytes" "key" {
  for_each = var.generated_keys

  length = 32
}

module "generated" {
  source   = "../secret"
  for_each = var.generated_keys

  project_id = var.project_id
  region     = var.region
  secret_id  = lower(replace(each.key, "_", "-"))
  labels     = var.labels

  mode             = "generated"
  value_version    = var.keys_version
  value_wo         = ephemeral.random_bytes.key[each.key].base64
  accessor_members = var.accessor_members
}

module "external" {
  source   = "../secret"
  for_each = var.external_secrets

  project_id = var.project_id
  region     = var.region
  secret_id  = lower(replace(each.key, "_", "-"))
  labels     = var.labels

  mode             = "external"
  accessor_members = var.accessor_members
}
