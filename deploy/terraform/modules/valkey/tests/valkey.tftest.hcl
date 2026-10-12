# Offline: google is mocked, nothing here reaches Google Cloud.
mock_provider "google" {
  # What Memorystore reports once the instance exists: one primary endpoint.
  mock_resource "google_memorystore_instance" {
    defaults = {
      endpoints = [{
        connections = [{
          psc_auto_connection = [{
            connection_type = "CONNECTION_TYPE_PRIMARY"
            ip_address      = "10.20.1.2"
            port            = 6379
          }]
        }]
      }]
    }
  }
}

variables {
  project_id = "kiwi-test-123"
  network_id = "projects/kiwi-test-123/global/networks/kiwi"
}

run "cluster_disabled_single_node_the_client_can_use" {
  command = plan

  assert {
    condition     = google_memorystore_instance.this.mode == "CLUSTER_DISABLED"
    error_message = "the instance must be cluster-disabled: the app's single-node client runs multi-key commands a cluster-mode instance refuses, and the mode cannot be changed later"
  }
  assert {
    condition     = google_memorystore_instance.this.shard_count == 1 && google_memorystore_instance.this.replica_count == 0
    error_message = "one node: one shard, no replica"
  }
  assert {
    condition     = google_memorystore_instance.this.node_type == "SHARED_CORE_NANO"
    error_message = "the smallest node type is the default"
  }
}

run "tls_and_iam_auth_are_off_because_the_client_supports_neither" {
  command = plan

  assert {
    condition     = google_memorystore_instance.this.transit_encryption_mode == "TRANSIT_ENCRYPTION_DISABLED"
    error_message = "the app's client dials plain TCP: in-transit encryption must be off until the client has a CA pool"
  }
  assert {
    condition     = google_memorystore_instance.this.authorization_mode == "AUTH_DISABLED"
    error_message = "the app's client has no token source: IAM auth must be off until it has one"
  }
}

run "private_endpoint_in_the_stack_network_and_protected_from_deletion" {
  command = plan

  assert {
    condition     = one(google_memorystore_instance.this.desired_auto_created_endpoints).network == var.network_id
    error_message = "the Private Service Connect endpoint must be created in the stack's VPC"
  }
  assert {
    condition     = google_memorystore_instance.this.deletion_protection_enabled == true
    error_message = "the instance must refuse deletion by default"
  }
}

run "url_is_the_primary_endpoint_without_credentials" {
  command = apply

  assert {
    condition     = output.url == "redis://10.20.1.2:6379"
    error_message = "KIWI_VALKEY_URL must be redis://<primary ip>:<port>, got ${output.url}"
  }
  assert {
    condition     = !can(regex("@", output.url))
    error_message = "the URL must carry no credential"
  }
}
