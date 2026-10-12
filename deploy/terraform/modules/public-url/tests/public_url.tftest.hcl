# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

variables {
  project_id = "kiwi-test-123"
}

run "default_url_is_the_deterministic_run_app_url" {
  command = apply

  assert {
    condition     = output.public_url == "https://kiwi-app-123456789012.asia-southeast1.run.app"
    error_message = "the default origin must be https://<service>-<project number>.<region>.run.app, got ${output.public_url}"
  }
  assert {
    condition     = output.oauth_redirect_uri == "https://kiwi-app-123456789012.asia-southeast1.run.app/api/auth/callback"
    error_message = "the redirect URI must be the origin plus /api/auth/callback"
  }
  assert {
    condition     = output.uses_custom_domain == false
    error_message = "no custom domain was given"
  }
}

run "a_custom_domain_replaces_the_default_without_other_change" {
  command = apply

  variables {
    custom_domain = "kiwi.example.com"
  }

  assert {
    condition     = output.public_url == "https://kiwi.example.com" && output.oauth_redirect_uri == "https://kiwi.example.com/api/auth/callback"
    error_message = "a custom domain must become the origin and the redirect URI base"
  }
}

run "the_origin_has_no_trailing_slash_port_or_path" {
  command = apply

  assert {
    condition     = !endswith(output.public_url, "/") && !can(regex(":[0-9]+$", output.public_url)) && !can(regex("^https://[^/]+/", output.public_url))
    error_message = "the app compares the browser's Origin with KIWI_PUBLIC_URL exactly: no trailing slash, port or path"
  }
}

run "custom_domain_with_a_scheme_is_refused" {
  command = plan

  variables {
    custom_domain = "https://kiwi.example.com"
  }

  expect_failures = [var.custom_domain]
}

run "custom_domain_with_a_path_or_port_is_refused" {
  command = plan

  variables {
    custom_domain = "kiwi.example.com:443"
  }

  expect_failures = [var.custom_domain]
}
