# Unit 7: the Cloud Run service (web ingress + kiwi sidecar), the migrate and seed Jobs, and the
# public invoker grant. The first apply starts from placeholder images; CI owns them afterwards.
include "root" {
  path   = find_in_parent_folders("root.hcl")
  expose = true
}

terraform {
  source = "${include.root.locals.modules_dir}//cloudrun-app"
}

dependencies {
  # The registry holds the images the service will run; nothing is read from it.
  paths = ["../apis", "../registry"]
}

dependency "iam" {
  config_path  = "../iam"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    run_service_account_email  = "kiwi-run@kiwi-offline-check.iam.gserviceaccount.com"
    jobs_service_account_email = "kiwi-jobs@kiwi-offline-check.iam.gserviceaccount.com"
    deploy_member              = "serviceAccount:kiwi-deploy@kiwi-offline-check.iam.gserviceaccount.com"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

dependency "network" {
  config_path  = "../network"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    network_name    = "kiwi"
    run_subnet_name = "kiwi-run"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

dependency "public_url" {
  config_path  = "../public-url"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    public_url = "https://kiwi-app-000000000000.asia-southeast1.run.app"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

dependency "storage" {
  config_path  = "../storage"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    bucket_url = "gs://kiwi-offline-check-kiwi-uploads"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

dependency "valkey" {
  config_path  = "../valkey"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    url = "redis://10.20.1.2:6379"
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

dependency "cloudsql" {
  config_path  = "../cloudsql"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    owner_secret_ids = {
      "KIWI_MIGRATE_DATABASE_URL" = "kiwi-migrate-database-url"
    }
    runtime_secret_ids = {
      "KIWI_DATABASE_URL"        = "kiwi-database-url"
      "KIWI_DATABASE_SYSTEM_URL" = "kiwi-database-system-url"
    }
    readonly_secret_ids = {
      "KIWI_DATABASE_RO_URL" = "kiwi-database-ro-url"
    }
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

dependency "secrets" {
  config_path  = "../secrets"
  skip_outputs = include.root.locals.offline

  mock_outputs = {
    secret_ids = {
      "KIWI_OAUTH_CLIENT_ID"     = "kiwi-oauth-client-id"
      "KIWI_OAUTH_CLIENT_SECRET" = "kiwi-oauth-client-secret"
      "KIWI_TOKEN_ENC_KEY"       = "kiwi-token-enc-key"
      "KIWI_BLOB_SIGNING_KEY"    = "kiwi-blob-signing-key"
    }
  }
  mock_outputs_allowed_terraform_commands = ["init", "validate", "providers"]
}

inputs = {
  project_id   = include.root.locals.project_id
  region       = include.root.locals.region
  service_name = include.root.locals.service_name

  run_service_account_email  = dependency.iam.outputs.run_service_account_email
  jobs_service_account_email = dependency.iam.outputs.jobs_service_account_email
  deploy_member              = dependency.iam.outputs.deploy_member

  network_name = dependency.network.outputs.network_name
  subnet_name  = dependency.network.outputs.run_subnet_name

  web_image  = include.root.locals.web_image
  kiwi_image = include.root.locals.kiwi_image

  public_url = dependency.public_url.outputs.public_url
  blob_url   = dependency.storage.outputs.bucket_url
  valkey_url = dependency.valkey.outputs.url

  # What the service and the seed Job read: the app and system connection strings and the
  # application secrets. The owner's and the read-only strings go to the migrate Job alone.
  runtime_secret_ids  = merge(dependency.cloudsql.outputs.runtime_secret_ids, dependency.secrets.outputs.secret_ids)
  owner_secret_ids    = dependency.cloudsql.outputs.owner_secret_ids
  readonly_secret_ids = dependency.cloudsql.outputs.readonly_secret_ids

  vertex_location           = include.root.locals.vertex_location
  vertex_embedding_location = include.root.locals.vertex_embedding_location
  vertex_model_flash        = include.root.locals.vertex_model_flash
  vertex_model_pro          = include.root.locals.vertex_model_pro
  vertex_embedding_model    = include.root.locals.vertex_embedding_model

  labels = include.root.locals.labels
}
