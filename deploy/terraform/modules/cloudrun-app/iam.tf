# Anyone may call the service: the app authenticates its own users (Google sign-in, session
# cookie, Origin check). Cloud Run's own invoker check would only keep the browser out.
resource "google_cloud_run_v2_service_iam_member" "public" {
  count = var.allow_public_access ? 1 : 0

  project  = var.project_id
  location = google_cloud_run_v2_service.this.location
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# The deploy identity may roll out revisions of the service and update and run the two Jobs, on
# these resources alone. The Jobs have no invoker grant for anyone: they are executed by people
# and by the deploy workflow through the Cloud Run API, never over HTTP.
resource "google_cloud_run_v2_service_iam_member" "deployer" {
  count = var.deploy_member == "" ? 0 : 1

  project  = var.project_id
  location = google_cloud_run_v2_service.this.location
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.developer"
  member   = var.deploy_member
}

resource "google_cloud_run_v2_job_iam_member" "deployer" {
  for_each = var.deploy_member == "" ? {} : {
    migrate = google_cloud_run_v2_job.migrate.name
    seed    = google_cloud_run_v2_job.seed.name
  }

  project  = var.project_id
  location = var.region
  name     = each.value
  role     = "roles/run.developer"
  member   = var.deploy_member
}
