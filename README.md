# K.I.W.I.

**Knowledge, Intelligence & Workflow Integration** is a team workspace (channels and real-time chat) with AI agents built in. It turns meetings, uploaded files and conversations into summaries, draft tasks and answers that cite their sources, in English and Thai. Nothing the AI drafts takes effect until a person confirms it.

Built for the AI Builder Cup 2026. **Today the whole product runs on one machine**, in a k3d cluster driven by Tilt. **Nothing is deployed to Google Cloud:** the infrastructure code for it is written and checked offline, and the app cannot serve production yet ([Deploy to Google Cloud](#deploy-to-google-cloud-prepared-not-applied)). All demo content is synthetic: the people, channels, messages, documents, recordings and meetings, and every address in this README, are invented.

> **How claims are labelled.** Every statement about how something was checked says whether it was **automated**, **simulated** or **real-condition**; the terms are defined in [What was checked, and how](#what-was-checked-and-how). What is only designed or prepared is called that, never "working".

[Quick start](#quick-start) · [How it works](#how-it-works) · [Run it locally](#run-it-locally) · [Google Cloud](#deploy-to-google-cloud-prepared-not-applied) · [What was checked](#what-was-checked-and-how) · [Limitations](#limitations) · [Layout](#repository-layout)

## What it does: the five demo flows

| # | Flow | What you see |
|---|---|---|
| 1 | Sign in and chat | Sign in with Google and land in `#general`. A message in English or Thai reaches the other member without a reload. A message sent offline waits, says so, and arrives once when the connection is back. |
| 2 | Meeting summary and action items | A recording uploaded to a channel becomes a summary slip (decisions, open questions, action items) plus AI drafts of the tasks. A Google Meet started from a channel is built to feed the same pipeline, but no real Meet transcript has been obtained ([Limitations](#limitations)), so the demo runs this flow on the recording upload. |
| 3 | AI-drafted tasks on the Board | The drafts wait for a person. Pick an owner and press Confirm: the stub tears away, the slip is sealed, and the task lands in To do, from where a menu moves it on. |
| 4 | Cited answers in English and Thai | Ask a question on the Ask page. The answer is a slip with a REF code and clickable sources; a Thai question gets a Thai answer. Each Ask is answered from a search made for that question, so an answer always carries its sources. When nothing is found, the not-found slip says so and names the channels searched. A non-member gets "not found in channels you can access", never the content. |
| 5 | Instant file summaries | Attach a file to a channel and its summary slip arrives with a REF code. An unsupported type is refused where it is attached; a scan with no text layer fails with its reason and a Retry button. |

Every AI result is a **slip**: it carries its sources and a REF code (`MTG-…` meeting, `DOC-…` file, `ANS-…` answer, `TSK-…` task). A failed attempt shows its reason with Retry and issues no REF.

<details>
<summary>Which channels an answer searches</summary>

An @mention of the assistant in a channel and an Ask opened from a channel search only that channel: in such a run the assistant's search, channel-summary and task tools refuse any other channel (checked by **automated** tests of the tools). That limits what the tools read; it does not cover what the model writes from its own conversation or session memory. An Ask opened from the Ask panel without a channel searches every channel the person belongs to. A same-topic question about content that lives in another channel therefore answers "not found" in the wrong channel: ask in the right channel, or from the Ask panel.

</details>

## Quick start

Run the product locally with the mock sign-in. You need Docker (running), [mise](https://mise.jdx.dev), Chromium or Firefox, and the endpoint, key and model ids of an OpenAI-compatible LiteLLM gateway that your team provides.

```bash
mise install                    # install the pinned tools (Go, Node, pnpm, task, k3d, Tilt, ...)
task setup                      # install the git hooks and the JavaScript dependencies
bash scripts/local/gen-env.sh   # create .env and generate the local secrets
# now edit .env: the LiteLLM base URL, key and three model ids
task local:up                   # k3d cluster + Tilt with the mock sign-in; stays in the foreground
task seed                       # in a second terminal: the demo workspace, with alice and bob
# open http://localhost:8080 and press "Continue with Google": a mock chooser appears
```

Details, real Google sign-in and tests: [Run it locally](#run-it-locally). `task local:down` stops it and deletes the cluster and all local data.

## How it works

### Local development

Everything runs in a k3d cluster on your machine, published on `127.0.0.1:8080` only. By default (the `mockauth` overlay) people sign in through a mock provider; with `OVERLAY=local` they sign in through real Google, and Meet comes with it.

```mermaid
flowchart LR
    you["Browser<br/>127.0.0.1:8080"]
    tilt["Your editor + Tilt"]
    subgraph k3d["k3d cluster on your machine"]
        traefik["Traefik<br/>ingress"]
        web["web<br/>Caddy + built app"]
        kiwi["kiwi, one binary<br/>api, realtime, worker, agents<br/>in-process bus"]
        mock["mockoidc<br/>mock sign-in"]
        pg[("Postgres<br/>+ pgvector")]
        vk[("Valkey")]
        vol[("Uploads<br/>volume")]
        ollama["Ollama<br/>embeddings"]
    end
    llm["LiteLLM<br/>chat + speech<br/>yours, external"]
    goog["Google sign-in + Meet<br/>OVERLAY=local only"]
    you --> traefik
    traefik -->|"/"| web
    traefik -->|"/api, /ws"| kiwi
    traefik -->|"/mockoidc"| mock
    kiwi --> pg & vk & vol & ollama & llm
    kiwi -.-> goog
    tilt -.->|"on save: rebuild and redeploy"| k3d
```

**What to notice:** one ingress sends `/` to the web app, `/api` and `/ws` to `kiwi`, and `/mockoidc` to the mock provider. Only LiteLLM and Google are outside the cluster.

- **One binary, one outbox.** `kiwi` runs four roles (api, realtime, worker, agents) in one process with an in-process message bus. Work is queued through a transactional outbox in Postgres, so a restart in the middle of a job redelivers it instead of losing it ([one job, start to finish](#one-job-start-to-finish)).
- **Data and models.** Postgres isolates workspaces with row-level security. Valkey holds sessions, realtime tickets and the fan-out of live events. Uploads live on a volume, served through signed URLs. Chat and speech-to-text come from your LiteLLM gateway; embeddings (768 dimensions) come from Ollama in the cluster, on the CPU.
- **Meet** has no push delivery in this phase: the worker polls the Meet REST API every 30 seconds for the meetings you started from K.I.W.I.
- **Dev loop.** `task local:up` runs Tilt. On save, Tilt rebuilds the `kiwi` binary or the `web` image, pushes it to the cluster's registry and redeploys it. There is no hot reload: the rebuild is the path.

### Production on Google Cloud

**Prepared, not applied.** The Terraform and Terragrunt code exists and is checked offline. No resource exists, nothing has been planned against Google Cloud, and the app cannot serve production yet. This is the design that code builds.

```mermaid
flowchart LR
    you["Browser"]
    gha["GitHub Actions<br/>deploy workflow: not written yet"]
    google["Google sign-in + Meet"]
    subgraph gcp["Google Cloud, asia-southeast1: PREPARED, NOT APPLIED"]
        direction LR
        subgraph run["Cloud Run service: exactly 1 instance, CPU always on"]
            direction LR
            web["web<br/>Caddy ingress container"]
            kiwi["kiwi sidecar<br/>api, realtime, worker, agents"]
        end
        jobs["Cloud Run Jobs<br/>kiwi-migrate, kiwi-seed"]
        sql[("Cloud SQL<br/>PostgreSQL 17, private IP")]
        vk[("Memorystore<br/>for Valkey")]
        gcs[("Cloud Storage<br/>uploads")]
        vertex["Vertex AI<br/>Gemini"]
        sm["Secret Manager"]
        ar["Artifact Registry"]
    end
    you -->|"HTTPS"| web
    web -->|"/api, /ws on localhost"| kiwi
    kiwi --> sql & vk & gcs & vertex & google
    jobs --> sql
    sm -.->|"secrets at start"| run
    sm -.-> jobs
    gha -->|"keyless, via Workload Identity Federation"| ar
    ar -.->|"images"| run
    classDef missing stroke-dasharray: 5 4
    class gcs,vertex missing
```

**What to notice:** only the Caddy ingress container is reachable from outside; `kiwi` sits behind it. Cloud SQL and Memorystore have private addresses only. The dashed boxes are parts the app cannot use yet: it has no Vertex AI provider and no Cloud Storage blob store.

- **Exactly one instance.** The message bus is in-process, and the Meet poll, the outbox relay and delayed jobs run inside the process, so a second instance would run them twice. A rollout briefly runs two. Meet works as it does locally: the worker polls, nothing pushes.
- **No keys.** Three service accounts without keys. GitHub Actions deploys through Workload Identity Federation, which accepts only this repository's `main` branch.

### One job, start to finish

The core idea: a job survives a crash, and nothing the AI drafts becomes a task until a person confirms it. This follows one recording upload (flows 2 and 3).

```mermaid
sequenceDiagram
    autonumber
    participant B as Browser
    participant K as kiwi
    participant P as Postgres
    participant M as Model
    participant V as Valkey
    B->>K: upload a recording
    K->>P: save the meeting and its outbox row, one transaction
    K-->>B: accepted, a pending slip shows
    K->>P: the relay claims the row and keeps it locked
    K->>M: transcribe, then summarize
    Note over K,P: Crash or restart here: the lock is released and the row is delivered again
    K->>P: save the summary slip with its REF, queue the task drafts, release the row
    K->>P: the queued job saves the drafts as drafts
    K->>V: publish live frames
    V-->>B: the slip arrives without a reload, drafts wait on the Board
    B->>K: pick an owner, Confirm
    K->>P: the draft becomes a task in To do
```

**What to notice:** the meeting and its job are saved together (step 2), and the row stays locked until the work is done (steps 4 to 6), so a crash puts the job back in the queue. Delivery is at least once and each job writes its result once. After five failed deliveries the bus only logs a dead letter (there is no dead-letter queue); the file and meeting jobs also mark the item failed, with a reason. Only a person's Confirm (steps 10 and 11) makes a task.

### Local and production side by side

| | Local (built and checked) | Production (prepared, not applied) |
|---|---|---|
| Runtime | k3d cluster, Tilt; `web` and `kiwi` are separate pods behind Traefik | One Cloud Run service: Caddy ingress container plus the `kiwi` sidecar |
| Database | Postgres + pgvector in the cluster | Cloud SQL, PostgreSQL 17, private IP, TLS, backups |
| Cache | Valkey in the cluster | Memorystore for Valkey, one cluster-disabled node |
| Uploads | A volume on the `kiwi` pod, signed URLs served by `kiwi` | A private Cloud Storage bucket (app adapter not built) |
| Models and embeddings | Your LiteLLM for chat and speech; Ollama `embeddinggemma` on the CPU | Vertex AI Gemini (`gemini-3.5-flash`, `gemini-embedding-001` by default; app provider not built) |
| Sign-in | Mock provider; real Google with `OVERLAY=local` | Google, with an OAuth web client you create; same invite-only rule |
| Meet events | The worker polls every 30 seconds | The same poll, inside the one instance |
| Secrets | Git-ignored `.env`, loaded into a Secret of the cluster | Secret Manager, one secret per variable |
| Deploy | `task local:up`; Tilt rebuilds on save | Terragrunt apply by the owner; images by GitHub Actions (workflow not written) |
| Scale | One `kiwi` pod | Exactly one instance (min = max = 1), CPU always on |

## Run it locally

Steps 1 to 5 run the product with the mock sign-in. Run the commands in a shell where mise is activated, or prefix them with `mise exec --`.

1. **Install the tools.** You need Docker (running), [mise](https://mise.jdx.dev) and Chromium or Firefox. `mise install` installs the pinned tools of `mise.toml` (Go, Node, pnpm, task, k3d, Tilt, kubectl, kustomize and more); `task setup` installs the git hooks (they refuse private paths and secrets before a commit) and the JavaScript dependencies. `task smoke` and `task reset` also need GNU coreutils `timeout` (on macOS, `brew install coreutils` provides `gtimeout`, which the scripts use when there is no `timeout`).
2. **Fill in `.env`.** `bash scripts/local/gen-env.sh` creates it from `.env.example` (git-ignored, mode 600) and generates the three local secrets. Set your LiteLLM base URL (without `/v1`: K.I.W.I. appends the API path), key and three model ids; the block "The `.env` variables" below lists every variable. No secret value is ever written into a manifest or committed.
3. **Start the cluster.** `task local:up` creates (or starts) the k3d cluster `kiwi`, loads `.env` into it and runs Tilt with the `mockauth` overlay. Tilt builds the images and deploys Postgres, the migration job, Valkey, Ollama (the first start downloads the embedding model), then `kiwi` and `web`. It stays in the foreground: use a second terminal, and wait until Tilt shows every resource ready.
4. **Seed the demo workspace.** `task seed` seeds the workspace `demo.example` with its channels, messages, documents and AI drafts, and invites alice and bob. `task seed -- --help` lists the flags.
5. **Open the app.** Go to `http://localhost:8080` and press **Continue with Google**. The default stack (`OVERLAY=mockauth task local:up` says it explicitly) needs no Google client: the button goes through a mock sign-in provider on the same origin, with a chooser of three accounts: `alice@demo.example` (has a hosted domain, `demo.example`: signs in), `bob@demo.example` (no hosted domain, invited by the seed: signs in) and `carol@elsewhere.example` (no hosted domain, not invited: refused with K.I.W.I.'s "has not been invited" page). Use this stack if you are not a test user of the team's Google OAuth app, or not seeded.

`task local:down` deletes the cluster, its registry and its volumes (so all local data).

The app is published on `127.0.0.1:8080` only: the default stack signs anyone in without a password, and the cluster holds your LLM key. The Kubernetes API port is bound to `127.0.0.1` too. From another machine, tunnel with `ssh -N -L 8080:127.0.0.1:8080 <host>` and open `http://localhost:8080` there: the browser origin has to stay `http://localhost:8080`, which the OAuth redirect URI requires. **Troubleshooting:** if the k3d agent node stays `NotReady` after a Docker or host restart, with "failed to find interface with specified node ip", `docker restart k3d-kiwi-agent-0` fixes it (seen once, on rootless Docker: **real-condition**).

<details>
<summary>The <code>.env</code> variables</summary>

`.env.example` lists every variable name with an empty value; a test keeps it in step with the code. The Secret the cluster reads is built from `.env` by `scripts/local/load-env-secret.sh`.

| Variable | Where it comes from |
|---|---|
| `KIWI_LLM_BASE_URL`, `KIWI_LLM_API_KEY` | Your team's LiteLLM gateway, shared out of band. The base URL has no `/v1` suffix. |
| `KIWI_LLM_MODEL_FLASH`, `KIWI_LLM_MODEL_PRO`, `KIWI_ASR_MODEL` | Model ids that your LiteLLM serves: two chat models and a speech-to-text model. |
| `KIWI_TOKEN_ENC_KEY`, `KIWI_BLOB_SIGNING_KEY`, `KIWI_DB_PASSWORD` | Generated by `gen-env.sh` when empty (`task local:up` runs it too). A value that is already set is never touched. |
| `KIWI_OAUTH_CLIENT_ID`, `KIWI_OAUTH_CLIENT_SECRET` | The Google OAuth client, needed only for [real Google sign-in](#real-google-sign-in). The default stack signs in through a mock provider and fills in a placeholder itself: leave them empty. |
| `KIWI_WORKSPACE_NAME`, `KIWI_WORKSPACE_DOMAIN` | Optional; they only change the sign-in heading and its domain hint. |
| The rest of `.env.example` | Set by the overlay in `deploy/k8s/overlays`: leave empty. |

</details>

<details>
<summary>Port bindings, older clusters and <code>localhost</code></summary>

A cluster created before these bindings existed keeps its old ones (every interface; k3d publishes the Kubernetes API port that way by default) until it is recreated with `task local:down` and `task local:up` (that deletes its data); `task local:up` prints a WARNING for each port it finds public on such a cluster. On a cluster recreated from the new configuration both ports were on `127.0.0.1` only (`docker port`, `ss -ltn`) and `kubectl` worked through the kubeconfig k3d wrote for it (**real-condition**).

On that host `localhost` resolves to `::1` first, which is refused because the binding is IPv4 only, so `http://localhost:8080` reaches the app through the client's fallback to `127.0.0.1` (Node's `fetch` and `http` and the headless Chromium of `task smoke` did). A client without that fallback fails on `localhost` and cannot work around it by switching the host: the OAuth redirect URI requires `localhost`. The clients named are the ones that were tried.

</details>

### Real Google sign-in

Sign-in is **invite-only**: only an address that already has a user row can sign in, and `kiwi seed --members <emails>` creates the rows (there is no invite flow yet). Any other Google account gets K.I.W.I.'s "has not been invited" page. A token with a Google Workspace hosted domain (`hd`) must match the domain of an existing workspace; sign-in never creates one. After setting up the Google side (block below), stop Tilt (Ctrl-C where `task local:up` runs) and start the stack with the real sign-in:

```bash
OVERLAY=local task local:up
task seed -- --members you@gmail.com
```

<details>
<summary>Setting up the Google side, and what <code>--members</code> seeds</summary>

1. Use one Google Cloud project.
2. Configure the OAuth consent screen as **External** with publishing status **Testing**, and add every person who will sign in as a **test user**. Google stops anyone else with `access_denied`.
3. Request the scopes `openid`, `email`, `profile` and `https://www.googleapis.com/auth/meetings.space.created`.
4. Enable the Google Meet REST API.
5. Create an OAuth client of type **Web application** with the redirect URI `http://localhost:8080/api/auth/callback`.
6. Put its client id and secret into the git-ignored `.env` as `KIWI_OAUTH_CLIENT_ID` and `KIWI_OAUTH_CLIENT_SECRET`.
7. Stop Tilt, then run the two commands above.

With `--members` alone, `task seed` creates the one workspace without a Google domain, called "Team". Two demo people, Nok and Pim, on the reserved `.invalid` top-level domain (no Google account can have such an address), own the channels and write the demo conversation; the addresses you list are plain members of the public channels and cannot read the private one. Separate several addresses with commas. Add `--domain <workspace domain>` (together with `--members`) only if your team has a Google Workspace domain.

Switching overlays keeps the data that is already in the cluster: a `demo.example` workspace seeded on the mock stack coexists with "Team". Three commands remove it: `task local:down`, and `task smoke` and `task reset` ([Testing](#testing)), which wipe the workspace data of the cluster, a real-Google "Team" workspace and its Meet grant included.

How this was checked: **real-condition**, by a human, once, through an External/Testing OAuth client: the consent screen, then the "has not been invited" page for a test user who was not yet seeded, then the sign-in of the invited test user. The `hd` (Workspace) branch is built and tested but cannot be exercised for real without a Workspace domain: **automated** and **simulated** checks only.

</details>

### Testing

- `task test` runs the Go tests with the race detector, then the TypeScript tests. The Go tests start real Postgres and Valkey containers, so Docker must be running.
- `task lint` checks Go formatting and runs golangci-lint and the TypeScript linters. `task gen:check` runs the generated-code drift check.
- `task check:scripts` runs the guard suites of the manifests and scripts. They need no cluster and no Docker, but network access: kubeconform downloads the Kubernetes schemas.
- `task smoke` runs the five flows in headless Chromium (desktop; flows 1 and 3 again at a 390x844 phone viewport) from a clean seed, with a recorded LLM and the mock sign-in. **It wipes the workspace data of the cluster**, a real-Google "Team" workspace and its Meet grant included, and seeds `demo.example`. `task smoke -- --list` lists the suite and touches nothing.
- `KIWI_LIVE_E2E=1 task smoke` runs the three `@live` tests (flows 2, 4 and 5) against the real LiteLLM and Ollama instead of the recorded LLM. It is opt-in and needs the five LLM values in `.env`.
- `task reset` wipes the workspace data of the running cluster and nothing else (run `task seed` afterwards). `task reindex` rebuilds the knowledge embeddings after you change the embedding model.

Pull requests run `.github/workflows/ci.yml` and the hygiene workflow (no private paths, no secrets, and the three hygiene suites). None of it builds an image or deploys. The CI jobs have run on GitHub: PR #1 and PR #2 ran `go`, `web`, `scripts`, `gen-drift`, `iac` and hygiene, and all of them passed after fixes.

<details>
<summary>More about the test commands and CI</summary>

- **`task test`.** On rootless Docker, if starting containers fails with `bind: address already in use`, run `cd backend && go test -race -p 1 ./...` instead.
- **What `task check:scripts` needs.** Network access: kubeconform downloads the Kubernetes schemas from `raw.githubusercontent.com` (observed: with no network it reports an error for every resource). It needs kustomize, kubeconform, gitleaks and task on `PATH`, and caddy to validate the Caddyfile (`mise install` provides all of them; without caddy that check is skipped, loudly). `task gen:check` runs `task gen`, then a diff: stage generated files first.
- **`task smoke` and friends.** `task smoke` needs no model session. Flags after `--` go to Playwright. The suite never touches live Meet: flow 2 goes through the WAV recording upload, with a recorded transcript. `KIWI_LIVE_E2E=1 task smoke` is never part of the default run, and wipes and seeds the same way. `task reset` stops kiwi while the tables are truncated, flushes Valkey and starts kiwi again.
- **`task check:scripts` suites.** `scripts/local/render.test.sh` (the rendered overlays through kustomize and kubeconform, the Caddyfile through `caddy validate`, the Tiltfile, the cluster scripts); `scripts/hygiene/{check-paths,gitleaks,readme}.test.sh` (the private-path and secret guards, and this README: its commands exist and it holds no secret, private address or dead link); `scripts/smoke/run.test.sh` (the end of `task smoke` and `task reset`: the order of their steps, what they refuse, their exit status and last line, with every program they call replaced by a stub). The checks of the README suite that read your `.env` are skipped, loudly, where there is none.
- **`task smoke` in full.** It wipes every table of the application schemas and Valkey. It installs the JavaScript dependencies as the lockfile has them, builds the Playwright browsers with Nix when `PLAYWRIGHT_BROWSERS_PATH` is not set (elsewhere, point that variable at a Playwright browsers directory with the chromium headless shell that the pinned `@playwright/test` runs), and at the end removes the older Tilt-built images from the host (the newest of each repository, and every image a container uses, are kept; nothing else is pruned). A machine with the five LLM values (`KIWI_LLM_BASE_URL`, `KIWI_LLM_API_KEY`, `KIWI_LLM_MODEL_FLASH`, `KIWI_LLM_MODEL_PRO`, `KIWI_ASR_MODEL`) in `.env` ends on the `mockauth` overlay, wiped again and freshly seeded with the default seed (`demo.example`: alice and bob). A machine without them cannot start `mockauth`: it ends on the `smoke` overlay, still holding the replay's seed, and says so on its last line. `KIWI_LIVE_E2E=1 task smoke` ends on `mockauth`, freshly seeded.
- **Time limits.** `task smoke` and `task reset` put a limit on the steps that can wait on kiwi, the database or a model: the scripts' own steps use GNU coreutils `timeout --foreground`, so Ctrl-C reaches the command being waited for (the cost: when a limit expires, `timeout` signals only the command it started, not what that command started in turn), while Tilt and the `kubectl` waits carry their own `--timeout`.
- **CI jobs.** Go formatting, build and race tests (`go`); lint, typecheck and tests of the web workspaces (`web`); the `scripts` job, which runs `render.test.sh` and `run.test.sh` with kustomize, kubeconform and task built by `go install` at pinned versions; the `gen-drift` job, which runs `scripts/check-gen-drift.sh` with buf, sqlc and task built the same way; and the `iac` job, which runs `task iac:check` with Terraform, Terragrunt, TFLint and Trivy from actions pinned to a commit.

</details>

## Deploy to Google Cloud (prepared, not applied)

`deploy/terraform` holds the infrastructure for [the production design above](#production-on-google-cloud): reusable Terraform modules (`modules/`) and a Terragrunt stack (`live/`) that runs them, in a project and region of your choice (the plan targets `asia-southeast1`). **Nothing has been planned against Google Cloud or applied: no resource exists.** The code is checked offline, with every Google credential removed (`task iac:check`), so none of its claims about Google's APIs has run for real. **The binary of this phase cannot serve production yet:** it exits at startup when `KIWI_ENV` is prod, because the model provider a production deployment requires is not built.

### The path to production

1. **Change the app** (not done). It needs a Vertex AI model provider for chat, speech and embeddings; a Cloud Storage blob store (browser uploads through signed URLs); a production port and Caddyfile so one origin serves the app and proxies `/api` and `/ws` to `kiwi`; database role setup in `kiwi migrate` (Cloud SQL cannot grant `BYPASSRLS`); a production configuration guard; configurable database pools; a short, safe shutdown; and a few smaller fixes ([full list](#what-the-app-still-needs-before-real-images-can-serve)).
2. **Write the deploy workflow** (not written). GitHub Actions builds `web` and `kiwi`, pushes them to Artifact Registry and deploys, signing in as the `kiwi-deploy` account through Workload Identity Federation. The account and the federation are in the stack.
3. **First apply by the owner**, unit by unit, then the post-apply steps ([How to apply](#how-to-apply)).

**Estimated cost of the demo window** (18 Oct – 4 Dec 2026): about **$422 at 20 people and $564 at 50** for the setup this stack builds, or about **$475 and $616** with the larger Cloud SQL tier (1 vCPU, 3.75 GiB) that the team's price comparison recommends. These are gross Google list prices of 12 Oct 2026, before free tiers and credits, and an estimate, not a measurement: they assume that comparison's usage model (35 working days) and one always-on instance for the whole window. The stack builds `db-g1-small` (1.7 GB), which allows 50 connections while one instance needs 36 and a rollout briefly runs two, so `max_connections` is raised to 100; under load that could run it short of memory (an inference, not measured).

### How to apply

The owner applies it once, signed in with `gcloud auth application-default login` as the project Owner (no service-account key is ever used or created). In order: create the Terraform state bucket by hand (Terragrunt never creates it; versioning matters, because state is the only record of what exists); copy `deploy/terraform/live/env.hcl.example` to the git-ignored `env.hcl` and set `project_id`, `state_bucket` and `github_repository`; then apply unit by unit. **For every unit: plan to a file, review it, apply exactly that file, delete the file.** Never use `run --all apply`: it would apply without a reviewed plan. After the apply: add the OAuth client id and secret as Secret Manager versions, register the redirect URI the `public-url` unit prints, deploy the images, then run the `kiwi-migrate` and `kiwi-seed` Jobs. The commands are in the block below.

<details>
<summary>Apply and post-apply, step by step</summary>

**Before you start.** You need a Google Cloud project with billing enabled and you as its Owner. You need Terraform 1.11 or later (checked with 1.14) and Terragrunt (checked with 0.93): `mise install` gives both, and Terragrunt is told to use Terraform, not OpenTofu. Enable the two APIs the stack needs before it can enable the rest, once: `gcloud services enable serviceusage.googleapis.com cloudresourcemanager.googleapis.com --project <project id>`. The `apis` unit manages them afterwards.

1. **Create the state bucket by hand.** Terragrunt never creates it (`--backend-bootstrap` is never passed). Pick a globally unique name; versioning matters, because state is the only record of what exists:

   ```bash
   gcloud storage buckets create gs://<state bucket> --project <project id> --location asia-southeast1 \
     --uniform-bucket-level-access --public-access-prevention
   gcloud storage buckets update gs://<state bucket> --versioning
   ```

2. **Write your values.** `env.hcl` is git-ignored; the example lists names only. Set the three required values (`project_id`, `state_bucket`, `github_repository` as owner/name). Everything else is optional: `region`, `zone`, `custom_domain` (changes the app URL, the OAuth redirect URI and the bucket's CORS origin, and nothing else; mapping the name to the service is not part of this stack), `billing_account` (only then is a budget with alerts created), the Vertex AI location and model ids, and the two image names.

   ```bash
   cp deploy/terraform/live/env.hcl.example deploy/terraform/live/env.hcl
   ```

3. **Apply unit by unit.** For every unit: plan to a file, review it, apply exactly that file, delete it. Do not use `run --all apply`: it would apply without a reviewed plan. The order is `apis`; then `iam`, `network` and `public-url`; then `registry`, `storage`, `cloudsql`, `valkey` and `secrets`; then `cloudrun`. `budget` can go any time after `apis`. A unit whose dependencies are not applied stops with an error: mock outputs are used only to validate, never to plan. The `apis` unit waits a minute after enabling APIs so the next unit does not meet one that is not ready.

   ```bash
   cd deploy/terraform/live/apis
   terragrunt plan -out="$PWD/tfplan"
   terragrunt show "$PWD/tfplan"
   terragrunt apply "$PWD/tfplan"
   rm "$PWD/tfplan"
   ```

**After the apply**

1. **Add the OAuth client** (Terraform cannot create it). Create or reuse a Google OAuth web client, then add its id and secret as the first real versions of the two secrets, typing each value on standard input so it stays out of shell history (finish with Ctrl-D):

   ```bash
   gcloud secrets versions add kiwi-oauth-client-id --project <project id> --data-file=-
   gcloud secrets versions add kiwi-oauth-client-secret --project <project id> --data-file=-
   ```

   Terraform created each secret with a placeholder version so the first apply could start a revision; the version you add is the one the service reads.
2. **Register the redirect URI** the `public-url` unit prints (`terragrunt output -raw oauth_redirect_uri` in `deploy/terraform/live/public-url`) as an authorised redirect URI of that client, and add every person who should sign in as a test user of the consent screen (External, Testing).
3. **Deploy the images.** The first apply starts the service from Google's public hello image, so it needs no image of ours. Build and push `web` and `kiwi` to the repository the `registry` unit reports and deploy them with `gcloud run deploy` or CI. After that Terraform ignores the images: applying never rolls a deployed image back. No deploy workflow exists yet.
4. **Migrate, then seed**, once real images are deployed (executing a Job before that fails at once, by design): `gcloud run jobs execute kiwi-migrate --region asia-southeast1 --project <project id> --wait`, then the seed Job with the people to invite, using gcloud's alternate-delimiter syntax because the addresses contain commas: `--args='^|^seed|--members|you@example.com,teammate@example.com'`.

</details>

**Still open:** the Vertex AI location (the Singapore endpoint with `gemini-3.5-flash`, processed in Singapore, which is the default; or the global endpoint with newer models, where Google chooses where they run; it is the `vertex_location` value in `env.hcl`, and embeddings always need a region); a custom domain, if any (`custom_domain` in `env.hcl`); and whether GKE follows (`deploy/terraform/live/root.hcl` describes the seam; no GKE module exists).

### What the app still needs before real images can serve

The infrastructure sets variables that the current binary does not honour, or cannot run without something else. The ids (F01 and so on) also appear in comments of `deploy/terraform`; they come from an architecture review that is kept outside this repository.

<details>
<summary>The full list of app-side changes</summary>

- **A Vertex AI provider** (F01, F05, F45): none is built, and a prod process refuses every other provider. It must read `KIWI_VERTEX_PROJECT`, `KIWI_VERTEX_LOCATION`, `KIWI_VERTEX_EMBED_LOCATION` and `KIWI_VERTEX_ALLOW_GLOBAL`, refuse the global endpoint unless allowed, and the three LiteLLM and Ollama endpoint variables must stop being required of the worker and agents roles when the provider is Vertex (F01). The embedding width is fixed at 768 in three places (F05).
- **A Cloud Storage blob store** (F03, F24, F26): `KIWI_BLOB_URL` is a `gs://` bucket, and only `file://` is accepted today. Uploads must go from the browser to the bucket through signed URLs the service signs with its own identity, create-only. The bucket deletes anything left under `uploads/` after seven days, so an accepted upload must be moved out of that prefix or it is deleted.
- **Two ports and a proxy** (F07, F17): `KIWI_LISTEN_ADDR` for kiwi (the stack sets it to its own port); a production Caddyfile that listens on the port Cloud Run passes, proxies `/api/*` and `/ws` to `KIWI_UPSTREAM` and redirects any other host to the host of `KIWI_PUBLIC_URL`.
- **A database role bootstrap in `kiwi migrate`** (F02): Cloud SQL cannot grant `BYPASSRLS`, and Terraform creates only the table owner. The migrate step must create `kiwi_app`, `kiwi_system` and `kiwi_ro` with the passwords in the connection strings the Job receives, the migration must give `kiwi_system` fallback row-security policies on the three tables it reads, and the tests must stop creating the role with a real superuser. The owner is a member of `cloudsqlsuperuser`; whether that suffices for `CREATE EXTENSION vector` is unchecked.
- **A prod configuration guard** (F06): the prod environment required whenever `K_SERVICE` is set, an https public URL with no path, TLS in the database URLs, a `gs://` blob URL.
- **Pool sizes as configuration and idle connection cleanup** (F10): the pools are fixed at 20 + 16 per process, which is why `max_connections` is 100 on a 1.7 GB tier; with 10 + 6 the default would do.
- **Shutdown in under eight seconds, a schema gate and an advisory lock in the migrate step** (F08, F27), **the PDF reader's self-check on the second-generation environment** (F25), **`seed` and `reindex` allowed under prod** (F28), **log severity** (F35), **retrying the Google sign-in discovery at start** (F44), and **per-user limits on model use** (F19; the budget only alerts).

</details>

<details>
<summary>Not verified on Google Cloud, what the stack builds, outputs, and the offline check</summary>

**Not verified**, because nothing has been applied: that Cloud Run's Direct VPC egress reaches a Memorystore for Valkey endpoint (Google's pages disagree: prove it on a scratch revision first), that a `db-g1-small` holds `max_connections` of 100 under load (the memory risk is noted next to the flag), that the sidecar placeholder starts as configured, and the exact roles a deploy needs.

**What the stack builds.** One Cloud Run service (a Caddy ingress container and the `kiwi` sidecar, one instance, CPU always allocated, a one-hour request timeout, second generation, Direct VPC egress for private ranges only), two Cloud Run Jobs (`kiwi-migrate`, `kiwi-seed`), Cloud SQL for PostgreSQL 17 on the smallest tier that can work (`db-g1-small`, Enterprise edition, zonal, private IP only, TLS required, backups and deletion protection on, `max_connections` raised to 100 for two overlapping instances), Memorystore for Valkey (one cluster-disabled node, private endpoint), a private Cloud Storage bucket for uploads, Secret Manager (one secret per variable, generated values never in Terraform state), an Artifact Registry Docker repository, three keyless service accounts (`kiwi-run`, `kiwi-jobs`, `kiwi-deploy`), Workload Identity Federation that accepts only this repository's `main` branch, and an optional billing budget.

**Outputs.** Run `terragrunt output` in the unit's directory. No output holds a secret.

| What | Unit | Output |
|---|---|---|
| App URL | `cloudrun` | `public_url` (and `service_uri`, which must equal it) |
| OAuth redirect URI to register | `public-url` | `oauth_redirect_uri` |
| Workload Identity provider (`google-github-actions/auth`) | `iam` | `workload_identity_provider` |
| Deploy service account email | `iam` | `deploy_service_account_email` |
| Image registry path | `registry` | `repository_path` |
| Cloud SQL connection name | `cloudsql` | `connection_name` |
| Valkey endpoint | `valkey` | `url` |
| Uploads bucket | `storage` | `bucket_name` |

**Checking it.** `task iac:check` runs, with every Google credential removed and the state backend never initialised: formatting, then `init`, `validate` and `terraform test` of every module against a mocked Google provider, tflint (Terraform and Google rulesets), a Trivy configuration scan with no findings, and a render, `init` and `validate` of the whole Terragrunt stack against mock outputs. It plans and applies nothing. It needs `terraform`, `terragrunt`, `tflint` and `trivy` on `PATH` and network access (provider and ruleset downloads). Pull requests run it as the `iac` job.

</details>

## What was checked, and how

- **automated**: runs without a person and without a real external service: unit and integration tests, linters, scripts.
- **simulated**: runs against a stand-in: a fake Google client, the mock sign-in provider, recorded LLM replies, a headless browser viewport, jsdom.
- **real-condition**: runs on the actual cluster or host, against the real service (real Google, real Meet, a real model), or on a physical device. A run can mix labels: the real cluster of `task smoke` is real-condition, but its recorded LLM, mock sign-in and headless Chromium stay **simulated**.

| Claim | How | Level |
|---|---|---|
| Google sign-in, invite-only | A human, once: consent, the invite rejection, the sign-in of an invited test user | **real-condition** |
| Workspace (`hd`) sign-in branch | Go tests with a fake OIDC provider; flow 1 through the mock provider | **automated**, **simulated** only |
| Start meeting; the Meet tab with `authuser` | A human, against the real Meet API and a multi-account browser | **real-condition** |
| Meet offering a transcript to a personal Gmail account | A human, three calls: Meet offered no transcript | **real-condition**: unavailable |
| Meet transcript polling and ingestion | Go tests with a fake Google client | **simulated**: no real transcript ingested |
| The five flows in a browser | `task smoke` (recorded LLM, mock sign-in, headless Chromium), 3 runs on the k3d cluster, 9 tests each (6 desktop, 3 phone), all passed | **automated**, **simulated**: not a device |
| Go and TypeScript logic | `task test`: real Postgres and Valkey in containers, jsdom for the web | **automated** |
| The real LLM and embeddings | `KIWI_LIVE_E2E=1 task smoke`, twice: the 3 live tests passed | **real-condition** for the LLM and embeddings only; sign-in and browser still **simulated** |
| Loopback ports; web container hardening | Pinned by `render.test.sh`; observed on a recreated cluster and in a container run by hand | **automated**, plus **real-condition** on one host |
| A first start on an empty cluster | The steps of `task local:up`: the first run did not finish (fixed, pinned by a test); a second fresh cluster came up healthy in about five minutes | **real-condition**, **automated** (the pin) |
| CI on GitHub | PR #1 and PR #2 ran `go`, `web`, `scripts`, `gen-drift`, `iac` and hygiene; all passed after fixes | **automated** |
| Terraform modules and Terragrunt stack | `task iac:check`: 39 checks, no Google credential | **automated** only: never planned or applied |

<details>
<summary>Evidence in full</summary>

- **The five flows.** `task smoke` (the replay run: recorded LLM, mock sign-in, headless Chromium; desktop, and flows 1 and 3 also at 390x844) ran on the k3d cluster three times through the task itself (twice in a row, then once on a cluster recreated from the final tree): 9 tests each (6 desktop, 3 phone), all passed. Headless browsers, not a device.
- **The real LLM and embeddings.** `KIWI_LIVE_E2E=1 task smoke` ran twice (the second on a cluster recreated from the final tree), through the task itself on the k3d cluster: the 3 live tests passed (flow 2 with one real WAV transcription, flow 4 with four Ask runs in English and Thai that each ended in a slip with its REF, flow 5 with a file summary) and the end-of-run wipe and default seed completed.
- **Meet and a personal Gmail account.** A human, against the real Meet API, in three calls: the poll saw each conference start and end, Meet offered neither Transcripts nor "Take notes", and no transcript was exposed. The Meet tab opened with `authuser=<your email>` was checked by a human in a real browser signed in to several Google accounts (Google documents nothing about the parameter).
- **Ports bound to `127.0.0.1`.** `render.test.sh` pins both lines of the k3d configuration. On a cluster recreated from it: `docker port k3d-kiwi-serverlb` showed `80/tcp -> 127.0.0.1:8080` and `6443/tcp -> 127.0.0.1:<port>`, `ss -ltn` listed both on `127.0.0.1` only, `kubectl get nodes` worked through the kubeconfig k3d wrote (it points at `https://127.0.0.1:<port>`), a connection to this host's LAN address on 8080 was refused, and `/healthz` answered 200 on `127.0.0.1:8080` and on `localhost:8080` (`localhost` resolves to `::1` first, which is refused: the clients fell back to `127.0.0.1`). The SSH-tunnel route from another machine was not exercised.
- **The web container** (user 65532, port 8080, read-only root filesystem, no capabilities). `render.test.sh` pins the manifest, the Dockerfile and the Caddyfile. The image was built and run once by hand outside the cluster with `docker run --read-only --cap-drop ALL`: it answered `/healthz` on 8080 as user 65532, with the three headers. The base image `caddy:2-alpine` cannot start with every capability dropped (observed: `exec /usr/bin/caddy: operation not permitted`, because its binary carries a file capability), which the Dockerfile removes. In the recreated cluster the pod was Ready with no restart, its log had no `permission denied`, it ran as uid and gid 65532 with its permitted, effective and bounding capability sets empty (`CapPrm`, `CapEff` and `CapBnd` 0), `touch` failed with "Read-only file system" under `/usr/share/caddy` and `/etc/caddy` and worked in the three emptyDirs (`/data`, `/config`, `/tmp`); through the Ingress `/`, `/signin`, `/c/x`, `/board`, `/ask`, `/sw.js`, `/manifest.webmanifest` and `/healthz` answered 200, a missing `/assets/...` file 404, and the web responses carried `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: same-origin` and no `Content-Security-Policy` (responses of the API and the mock provider come from other pods and do not carry them).
- **A first start on an empty cluster** (overlay `mockauth`). The steps of `task local:up` up to its Tilt step, run on a freshly created cluster: `scripts/local/k3d-up.sh`, `scripts/local/gen-env.sh`, `scripts/local/load-env-secret.sh mockauth`, then `tilt ci -- --overlay mockauth` in place of the foreground `tilt up` (`task local:up` itself was not run on the empty cluster). The first `tilt ci` did not finish within 40 minutes: kiwi waited for the mock sign-in provider, whose pod sat in `CreateContainerConfigError` (`configmap "kiwi-config" not found`) because Tilt applied that ConfigMap together with the kiwi resource. It now has a resource of its own that both wait for; `render.test.sh` pins it (shown failing on the old Tiltfile, passing on the new one), and a second cluster created from nothing, with the same steps, came up with every workload healthy in about five minutes.
- **CI.** Before the GitHub runs, `task check:scripts` and the generated-code check passed locally; the `go install` lines of the jobs (kustomize, kubeconform, task, caddy, buf, sqlc) were run locally, and the two suites of the `scripts` job passed on a runner-like `PATH` that holds only those tools, with `GITHUB_ACTIONS=true` (where a missing caddy fails the suite).
- **Terraform and Terragrunt.** `task iac:check`: 39 checks, all passed, run with every Google credential removed: formatting, `init`, `validate` and `terraform test` of the 12 modules against a mocked Google provider (the two modules that generate secrets use the real `random` provider, which only computes locally), tflint, a Trivy scan with no findings, and a render, `init` and `validate` of the 11 Terragrunt units against mock outputs. Each invariant that a plausible mistake would break (no public Cloud SQL address, TLS, deletion protection, backups, cluster-disabled Valkey, a private bucket, one always-on Cloud Run instance in prod with private-range egress, the repository and branch pin of the GitHub federation, no service-account key) was shown to fail its test when the setting was broken in a throwaway copy. Never planned or applied against Google Cloud, so no resource exists and the provider's schema checks are all that tested the arguments.
- **Hosts.** Every cluster run reported in this README was made on one x86-64 Linux machine with rootless Docker. The real-Google and `authuser` checks by a human drove that stack from Chrome on a Mac through an SSH tunnel; no macOS host has been tried. The kiwi binary is built for the architecture that `go env GOARCH` reports on the host (the cluster follows the host's Docker); no other host architecture and no rootful Docker has been tried. CI does not run the cluster runs: the default checks are `task test` and the pull-request workflows.

</details>

## Limitations

### Google sign-in and Meet

- **Real only where checked.** Sign-in, Start meeting and the Meet tab were checked against real Google by a human (**real-condition**); transcript polling and ingestion are **simulated** only, because no real transcript has ever been obtained.
- **Sign-in is invite-only.** There is no invite flow: only addresses seeded with `kiwi seed --members <emails>` can sign in, and sign-in never creates a workspace. The invite rule was checked **real-condition** once; the `hd` branch is **automated** and **simulated** only.
- **A seeded address can be claimed** by any Google account that proves it, until its owner first signs in. Check every address you pass to `--members`.
- **The app is External, in Testing status.** Only test users can sign in, and Google shows an "unverified app" warning. Google expires refresh tokens after 7 days, so Meet access needs re-consent about weekly: Start meeting sends you through Google's consent screen again (K.I.W.I. forces that screen so a lost grant can be given again).
- **No Meet transcript for a personal Gmail account** (**real-condition**). Meeting summaries come from the WAV recording upload, which is how flow 2 runs.
- **No push delivery.** A transcript is noticed up to 30 seconds (one poll interval) after Meet makes it available, on top of Meet's own delay. Meetings that were not started from K.I.W.I. are never seen, and if transcription is restarted inside one meeting, only the first transcript is ingested.
- **A Meet-started meeting has no failure surface in the web:** a failure is stored on the meeting, but the web shows neither it nor a Retry. If the host's token is revoked or expires, Start meeting sends the host through Google's consent screen again, and a meeting already being polled fails on the server with the reason `meet_access`. The recording upload shows its own failures with their reasons.
- **The Meet tab opens with `authuser=<your email>`**, so a browser signed in to several Google accounts joins as the space owner. A human verified this (**real-condition**), though Google documents nothing about the parameter, so it may stop working.

<details>
<summary>Google and Meet: the long explanations</summary>

- **An unadopted placeholder can be claimed.** A seeded address that has not signed in yet is a placeholder row: any Google account that proves that exact address, without a hosted domain, can claim it until the first sign-in binds the row to its Google subject. Placeholders never expire, and there is no unbind or admin path. The seed refuses an address that is already present in another workspace. The demo personas use the reserved `.example` and `.invalid` domains, which no Google account can verify. A mistyped real address in `--members` therefore stays claimable: the only cure today is `task reset` and a reseed, or fixing the row by hand.
- **The live-Meet transcript path is unavailable for a personal Gmail account** (verified by a human against the real Meet API in three calls: **real-condition**). Start meeting works and the poll saw each conference start and end, but in none of the three calls did Meet offer Transcripts or "Take notes" to the personal Gmail account that hosted the meeting, and no transcript was exposed: that is what was **observed**. It is probably because automatic transcription is a Google Workspace feature; that cause is an inference, not something observed. The Meet API itself refused, twice, to create a space with automatic transcription for this account (`FEATURE_UNAVAILABLE_TO_USER`). K.I.W.I. then creates the space without transcription and tells the host, in English and Thai, to turn it on in Meet if the account offers it. The Meet transcript path (polling, ingestion, summary) stays wired and is tested only against a fake Google client: it has never ingested a real transcript, and a Google Workspace account that offers transcription has not been tried. By design, a meeting with no transcript ends as `no_transcript` 30 minutes after it ends; that outcome has not been observed against real Meet.
- **Failures of a Meet-started meeting.** When one fails (for example `meet_access` or `meet_unavailable`), the server stores the reason on the meeting, but the web shows neither the failure nor a Retry. One that ends with no transcript only gets an assistant message in its channel asking for a recording (tested against a fake Google client; not observed against real Meet).

</details>

### Recordings, models and indexing

- **Recordings must be PCM16 WAV.** Phone recordings (m4a, webm, mp4) are not accepted locally: convert them first.
- **Upload limits.** A message attachment is a PDF, PNG, JPEG, plain-text, Markdown or CSV file of at most 25 MiB. A recording is a WAV file of at most 200 MiB: 16-bit PCM, mono or stereo, 8,000 to 96,000 Hz, with at least 250 ms of audio. A file of another type or over its limit is refused where it is attached; a recording that is not such a WAV fails with its reason.
- **Local transcripts have no speaker names and no timing.** Slips from a recording show no speaker names and no transcript offset links (the interface shows nothing rather than a false `0:00:00`), and tasks drafted from them cite the meeting without a transcript moment.
- **Speech-to-text is slow and shared.** It runs at about 5 times real time, and a bad upload can stall it for a minute, hence the strict WAV check. Thai/English code-switched audio is the target; Thai speech recognition quality has only been checked on synthetic speech.
- **Embeddings run on the CPU.** A large document takes minutes to index. A scanned PDF has no text layer and fails with its reason on this provider.

### The web app and devices

- **No Safari.** The session cookie uses the `__Host-` prefix and the stack is served over plain `http://localhost`; Safari is not supported.
- **No phone check.** A phone cannot use this stack over your network: the app port is bound to `127.0.0.1`, and the session cookie needs HTTPS off localhost anyway. The phone layout was checked only in headless Chromium at 390x844 (**simulated**); PWA install has not been checked.
- **Unsent messages are lost on reload.** A recording's pending or failed slip follows only the browser session that uploaded it: after a reload, a failed recording shows no slip.
- **The assistant has rough edges.** An @mention in a channel can still be answered in plain text with no source and no REF (only the Ask page always ends in a slip with its sources). The assistant advertises tools to the Ask page that cannot work there: a request to write a message or to list tasks, typed into Ask, ends as an unrelated sourced answer or as the not-found slip.
- **Not built:** typing and presence indicators, rate limits and spend budgets, Board drag and drop (cards move through a menu). No claim about users, connections or requests per second is made for this phase.

<details>
<summary>More on the web app</summary>

- The message `Meeting started: <link>` is posted under the `@assistant` label (the API has no system author kind) and is plain text: the other members must copy the link, and only the host's tab opens it.
- Caddy adds `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: same-origin` to every response of the web app. A Content-Security-Policy and the other browser security headers need a pass in a real browser (the PWA, its service worker, the self-hosted fonts) and are a GCP-phase item.

</details>

## Repository layout

| Path | Contents |
|---|---|
| `backend/` | The Go module: `cmd/kiwi` (the one binary: `serve`, `migrate`, `seed`, `reindex`), `internal/` (identity, chat, tasks, docs, meetings, knowledge, agents, realtime, platform), `db/` (migrations and queries), `tools/` (the mock sign-in provider and the LLM replay server used by the smoke run) |
| `apps/web/`, `packages/` | The web app (React, Vite) and its shared packages: `ui`, `core`, `api-client` |
| `proto/` | The protobuf contracts; generated code is committed |
| `deploy/` | The k3d cluster configuration and the kustomize base and overlays (`local`, `mockauth`, `smoke`); `terraform/` (modules and the Terragrunt stack for Google Cloud) |
| `e2e/` | The Playwright smoke suite |
| `scripts/` | Local-cluster, smoke, web and hygiene scripts |
| `Taskfile.yml`, `Tiltfile`, `mise.toml` | The task runner, the Tilt configuration and the toolchain pins |
