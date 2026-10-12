# The origin of the app, in one place, because three things must agree on it character for
# character: KIWI_PUBLIC_URL (the app's Origin check and the OAuth redirect it builds), the CORS
# origin of the uploads bucket, and the redirect URI registered in the Google OAuth client.
#
# Without a custom domain the origin is the deterministic Cloud Run URL
# https://<service>-<project number>.<region>.run.app (Cloud Run documents it next to the
# hash-based one). It is known before the service exists, which is what lets the OAuth client
# be registered, and the bucket configured, before the first deploy.
data "google_project" "this" {
  project_id = var.project_id
}

locals {
  default_host = "${var.service_name}-${data.google_project.this.number}.${var.region}.run.app"
  host         = var.custom_domain != "" ? var.custom_domain : local.default_host
}
