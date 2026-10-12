# One custom-mode VPC with no firewall rule that allows ingress: nothing here accepts a connection
# from outside. Cloud Run reaches Cloud SQL and Valkey through Direct VPC egress; both answer over
# private addresses only.
resource "google_compute_network" "this" {
  project                 = var.project_id
  name                    = var.network_name
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"
}

# Direct VPC egress. Cloud Run is configured with PRIVATE_RANGES_ONLY, so only traffic to private
# addresses (Cloud SQL, Valkey) enters this subnet; Vertex AI, Cloud Storage and Google sign-in
# go straight to the internet from Cloud Run and need no Cloud NAT.
resource "google_compute_subnetwork" "run" {
  project                  = var.project_id
  name                     = "${var.network_name}-run"
  region                   = var.region
  network                  = google_compute_network.this.id
  ip_cidr_range            = var.run_subnet_cidr
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_10_MIN"
    flow_sampling        = var.flow_log_sampling
    metadata             = "EXCLUDE_ALL_METADATA"
  }
}

# The address Memorystore for Valkey gets through Private Service Connect is allocated from this
# subnet by the service connection policy below.
resource "google_compute_subnetwork" "valkey" {
  project                  = var.project_id
  name                     = "${var.network_name}-valkey-psc"
  region                   = var.region
  network                  = google_compute_network.this.id
  ip_cidr_range            = var.valkey_subnet_cidr
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_10_MIN"
    flow_sampling        = var.flow_log_sampling
    metadata             = "EXCLUDE_ALL_METADATA"
  }
}

resource "google_network_connectivity_service_connection_policy" "valkey" {
  project       = var.project_id
  name          = "${var.network_name}-memorystore"
  location      = var.region
  service_class = "gcp-memorystore"
  network       = google_compute_network.this.id
  description   = "Lets Memorystore for Valkey create its Private Service Connect endpoint in ${google_compute_subnetwork.valkey.name}."

  psc_config {
    subnetworks = [google_compute_subnetwork.valkey.id]
  }
}

# Private services access for Cloud SQL: a reserved range and a peering with Google's service
# network. The instance gets its private address from the range.
resource "google_compute_global_address" "private_services" {
  project       = var.project_id
  name          = "${var.network_name}-private-services"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  address       = var.private_services_address
  prefix_length = var.private_services_prefix_length
  network       = google_compute_network.this.id
}

resource "google_service_networking_connection" "private_services" {
  network                 = google_compute_network.this.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_services.name]

  # Teardown: without this the peering outlives the connection and blocks deleting the network
  # (Cloud SQL keeps its producer resources for a while after an instance is deleted). It only
  # matters on destroy, and Cloud SQL (deletion protection on) has to be removed first anyway.
  deletion_policy = "REMOVE_PEERING"
}
