-- +goose Up

-- pgvector is not a trusted extension: the environment bootstrap (the local
-- postgres-init, Terraform on GCP, the test helper) creates it as a superuser.
-- This statement then finds it and does nothing, and fails loudly where the
-- bootstrap forgot.
CREATE EXTENSION IF NOT EXISTS vector;
