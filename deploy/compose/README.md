# K.I.W.I. with Docker Compose (one server, or Dokploy)

The minimal profile of the app on one host: one `kiwi` container serves the web app, `/api` and `/ws` (cache in
memory, sessions in Postgres), with Postgres + pgvector, Ollama (`embeddinggemma`) for embeddings, Gemini through
LiteLLM, and the mock sign-in provider (people pick a demo account; the workspace is synthetic demo data).

Checked on 18 Oct 2026 on a Mac (Colima), `--profile standalone` on `https://localhost:8443`: every service came up,
`kiwi seed` seeded and indexed the demo workspace, and a browser signed in as alice, opened #q4-launch with its meeting
summary, sent a message, listed Meetings and got a cited Ask answer (REF ANS-…) in 23 s.

## Environment

`make-env.sh <host> <file with GEMINI_API_KEY=...>` prints every value, with fresh secrets:

```bash
./make-env.sh kiwi.example.com ~/path/to/gemini.env
```

`.env.example` lists the names. The Gemini key is the only value you bring; LiteLLM spreads the requests over several
Gemini Flash models (`litellm/config.yaml`), because the free tier allows about 20 a day per model.

## On Dokploy

1. Install Dokploy on the server (Ubuntu 24.04, 4 vCPU / 8 GB or more), then open `http://<server ip>:3000`:
   ```bash
   curl -sSL https://dokploy.com/install.sh | sh
   ```
2. **Create Service → Compose**, provider GitHub, this repository, the branch with this folder, compose path
   `deploy/compose/docker-compose.yml`.
3. **Environment**: paste the output of `make-env.sh` for your domain (leave `HTTPS_PORT` and `HTTP_PORT` out).
   No domain? Use `<ip with dashes>.sslip.io`, for example `203-0-113-7.sslip.io`.
4. **Domains**, both on that host with HTTPS (Let's Encrypt):
   - path `/mockoidc` → service `mockoidc`, port `8090`
   - path `/` → service `kiwi`, port `8080`
5. **Deploy**. Then seed the demo workspace once, over SSH on the server:
   ```bash
   docker exec "$(docker ps -q --filter label=com.docker.compose.service=kiwi | head -1)" /app/kiwi seed
   ```
6. Open `https://<host>`: the landing page, then Sign in → Continue with Google → pick alice@demo.example.

## On a plain server

```bash
./make-env.sh <host> <gemini key file> > .env
docker compose --profile standalone up -d --build
docker compose exec kiwi /app/kiwi seed
```

`--profile standalone` adds Caddy on ports 80 and 443, which gets the certificate itself.

## What this stack is not

It is the demo shape, not the production design: `KIWI_ENV=local` with the mock sign-in, uploads on a Docker volume,
embeddings on the CPU and Gemini through the API key, not Vertex AI. Anyone who reaches the address can sign in as a
demo person and spend the Gemini key's free quota. Recordings need a speech model the gateway does not serve, so the
recording upload fails here; every other flow works.
