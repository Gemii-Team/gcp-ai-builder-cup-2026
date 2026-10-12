resource "google_artifact_registry_repository" "this" {
  project       = var.project_id
  location      = var.region
  repository_id = var.repository_id
  format        = "DOCKER"
  description   = "K.I.W.I. application images"
  labels        = var.labels

  # CI tags by git SHA and a re-run of a workflow pushes the same tag again, so tags stay mutable.
  docker_config {
    immutable_tags = false
  }

  # Storage beyond the free half gigabyte is billed, and every merge adds an image: keep the most
  # recent versions and drop untagged leftovers.
  cleanup_policy_dry_run = false

  cleanup_policies {
    id     = "delete-untagged"
    action = "DELETE"
    condition {
      tag_state  = "UNTAGGED"
      older_than = "${var.untagged_ttl_days * 86400}s"
    }
  }

  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = var.keep_recent_versions
    }
  }
}

# Pushing is the deploy account's alone. Cloud Run pulls with its service agent, which reads
# repositories of its own project without a grant.
resource "google_artifact_registry_repository_iam_member" "writer" {
  for_each = var.writer_members

  project    = var.project_id
  location   = google_artifact_registry_repository.this.location
  repository = google_artifact_registry_repository.this.name
  role       = "roles/artifactregistry.writer"
  member     = each.value
}
