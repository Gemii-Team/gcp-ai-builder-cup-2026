locals {
  services = setunion(var.services, var.extra_services)
}

data "google_project" "this" {
  project_id = var.project_id

  # Reading the project needs the Cloud Resource Manager API, which this module enables:
  # on the first apply the read waits for it.
  depends_on = [time_sleep.propagation]
}

resource "google_project_service" "this" {
  for_each = local.services

  project = var.project_id
  service = each.value

  # APIs outlive this stack: disabling one on destroy would break whatever else in the
  # project still uses it, and Terraform cannot know that.
  disable_on_destroy         = false
  disable_dependent_services = false
}

resource "time_sleep" "propagation" {
  create_duration = var.propagation_wait

  # Adding an API to the list waits again for it.
  triggers = {
    services = join(",", sort(tolist(local.services)))
  }

  depends_on = [google_project_service.this]
}
