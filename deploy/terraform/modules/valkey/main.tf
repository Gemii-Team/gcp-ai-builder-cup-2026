locals {
  zone = coalesce(var.zone, "${var.region}-a")

  connections = flatten([
    for endpoint in google_memorystore_instance.this.endpoints : [
      for connection in endpoint.connections : connection.psc_auto_connection
    ]
  ])
  # In cluster-disabled mode the node is reached through the primary endpoint.
  primary = [for connection in local.connections : connection if connection.connection_type == "CONNECTION_TYPE_PRIMARY"]
}

# The cache behind sessions, one-time realtime tickets, the members cache, OAuth state and the
# realtime fan-out (sharded pub/sub). It is ephemeral by design: losing it signs everyone out.
resource "google_memorystore_instance" "this" {
  project     = var.project_id
  location    = var.region
  instance_id = var.instance_id
  labels      = var.labels

  deletion_protection_enabled = var.deletion_protection

  # CLUSTER_DISABLED, and it cannot be changed after creation: Memorystore creates a cluster-mode
  # instance when the mode is left out. The app talks to one node with a plain single-node client
  # and runs multi-key commands (a Lua script and MULTI/EXEC over two keys of the members cache)
  # that a cluster-mode instance would refuse with CROSSSLOT (architecture review F15).
  mode        = "CLUSTER_DISABLED"
  shard_count = 1
  # One node, no replica: no SLA and no failover (shared-core-nano has none either way). A restart
  # of the node signs everyone out; F16 accepts that for the demo.
  replica_count = 0
  node_type     = var.node_type

  engine_version = var.engine_version

  # TLS and IAM authentication are off because the app's client supports neither: it parses a
  # redis:// URL and dials plain TCP, with no CA pool and no token source. The endpoint is a
  # private address inside this VPC, reachable only through Private Service Connect from
  # workloads in the network, so the exposure is the network's, which the network module closes.
  # To turn either on the client has to change first (KIWI_VALKEY_CA, architecture review F15);
  # both settings are immutable, so that means a new instance.
  transit_encryption_mode = "TRANSIT_ENCRYPTION_DISABLED"
  authorization_mode      = "AUTH_DISABLED"

  zone_distribution_config {
    mode = "SINGLE_ZONE"
    zone = local.zone
  }

  desired_auto_created_endpoints {
    network    = var.network_id
    project_id = var.project_id
  }

  lifecycle {
    postcondition {
      condition = length(flatten([
        for endpoint in self.endpoints : [
          for connection in endpoint.connections : [
            for psc in connection.psc_auto_connection : psc if psc.connection_type == "CONNECTION_TYPE_PRIMARY"
          ]
        ]
      ])) == 1
      error_message = "Expected exactly one primary endpoint on the cluster-disabled instance."
    }
  }
}
