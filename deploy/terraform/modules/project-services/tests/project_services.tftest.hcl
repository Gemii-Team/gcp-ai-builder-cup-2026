# Offline: every provider is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}
mock_provider "time" {}

variables {
  project_id = "kiwi-test-123"
}

run "every_api_the_stack_needs_is_enabled" {
  command = plan

  assert {
    condition = length(setsubtract(toset([
      "run.googleapis.com",
      "sqladmin.googleapis.com",
      "servicenetworking.googleapis.com",
      "compute.googleapis.com",
      "memorystore.googleapis.com",
      "networkconnectivity.googleapis.com",
      "secretmanager.googleapis.com",
      "artifactregistry.googleapis.com",
      "aiplatform.googleapis.com",
      "iam.googleapis.com",
      "iamcredentials.googleapis.com",
      "sts.googleapis.com",
      "cloudresourcemanager.googleapis.com",
      "storage.googleapis.com",
      "logging.googleapis.com",
      "monitoring.googleapis.com",
      "meet.googleapis.com",
    ]), toset(keys(google_project_service.this)))) == 0
    error_message = "an API the stack needs is missing: ${jsonencode(sort(keys(google_project_service.this)))}"
  }
}

run "apis_are_not_disabled_on_destroy" {
  command = plan

  assert {
    condition     = alltrue([for s in google_project_service.this : s.disable_on_destroy == false && s.disable_dependent_services == false])
    error_message = "destroying this stack must not disable an API other things in the project may use"
  }
}

run "extra_services_are_added_to_the_default_list" {
  command = plan

  variables {
    extra_services = ["workspaceevents.googleapis.com"]
  }

  assert {
    condition     = contains(keys(google_project_service.this), "workspaceevents.googleapis.com") && contains(keys(google_project_service.this), "run.googleapis.com")
    error_message = "extra_services must be enabled in addition to, not instead of, the defaults"
  }
}

run "propagation_wait_must_be_a_duration" {
  command = plan

  variables {
    propagation_wait = "soon"
  }

  expect_failures = [var.propagation_wait]
}

run "project_id_must_be_a_project_id" {
  command = plan

  variables {
    project_id = "Not A Project"
  }

  expect_failures = [var.project_id]
}
