# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {}

variables {
  project_id = "kiwi-test-123"
}

run "custom_vpc_with_a_cloud_run_subnet_large_enough" {
  command = plan

  assert {
    condition     = google_compute_network.this.auto_create_subnetworks == false
    error_message = "the VPC must be custom-mode: no subnet may appear in a region that was not asked for"
  }
  assert {
    condition     = tonumber(split("/", google_compute_subnetwork.run.ip_cidr_range)[1]) <= 26
    error_message = "Direct VPC egress needs a /26 or larger subnet"
  }
}

run "a_subnet_smaller_than_26_is_refused" {
  command = plan

  variables {
    run_subnet_cidr = "10.20.0.0/27"
  }

  expect_failures = [var.run_subnet_cidr]
}

run "private_services_access_for_cloud_sql" {
  command = plan

  assert {
    condition     = google_compute_global_address.private_services.purpose == "VPC_PEERING" && google_compute_global_address.private_services.address_type == "INTERNAL"
    error_message = "Cloud SQL private IP needs an internal VPC_PEERING range"
  }
  assert {
    condition     = google_service_networking_connection.private_services.service == "servicenetworking.googleapis.com"
    error_message = "the peering must be with the Service Networking producer network"
  }
}

run "valkey_gets_private_service_connect_in_its_own_subnet" {
  command = plan

  assert {
    condition     = google_network_connectivity_service_connection_policy.valkey.service_class == "gcp-memorystore"
    error_message = "the service connection policy must be for Memorystore"
  }
  assert {
    condition     = google_network_connectivity_service_connection_policy.valkey.location == var.region
    error_message = "the policy must be in the region of the instance"
  }
}

run "no_firewall_opens_the_network_to_the_outside" {
  command = plan

  module {
    source = "../../guards/forbidden-patterns"
  }

  assert {
    condition     = output.files_scanned > 20
    error_message = "the guard scanned only ${output.files_scanned} files: it is looking in the wrong place"
  }
  assert {
    condition     = length(output.offenders.open_ingress_firewall) == 0
    error_message = "a firewall rule must not allow ingress from 0.0.0.0/0, found in ${jsonencode(output.offenders.open_ingress_firewall)}"
  }
}
