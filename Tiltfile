# -*- mode: Python -*-
# The local stack on the k3d cluster. `task local:up` starts it:
#   tilt up -- --overlay mockauth
# mockauth, the default, signs in through the mock provider (backend/tools/mockoidc).
# `--overlay local` signs in with Google and needs the real KIWI_OAUTH_* of the
# kiwi-local client in .env. `--overlay smoke` is mockauth with the model replaced by
# recorded fixtures (backend/tools/llmreplay): kiwi sends its LLM and embedding calls
# to the replay server and no paid model session is used.
# `tilt ci -- --overlay <name>` brings the same stack up without the UI and leaves it
# deployed; running it again with another overlay switches the cluster over.
#
# Start order, on the first start only: postgres -> migrate -> kiwi,
# ollama -> ollama-pull -> kiwi, valkey -> kiwi, kiwi-config -> kiwi and, under mockauth
# and smoke, kiwi-config -> mockoidc -> kiwi and, under smoke, llmreplay -> kiwi.
# kiwi-config is the ConfigMap of that name on a resource of its own: mockoidc and kiwi both
# read it, and Tilt applies an object only after the resource that owns it has its
# dependencies (it used to belong to kiwi, which waits for mockoidc, whose pod then never
# started on a cluster that had not run the local overlay first). resource_deps make a
# resource wait until its dependencies are ready (a Job is ready once it has
# completed); they do not order later updates: after a code change kiwi and
# migrate are rebuilt and redeployed in parallel.
#
# The ConfigMap kiwi-config reaches the pod through envFrom, so editing it
# changes nothing in a running kiwi. Restart it to pick the change up:
#   kubectl --context k3d-kiwi -n kiwi rollout restart deploy/kiwi

KUBE_CONTEXT = 'k3d-kiwi'
REGISTRY = 'kiwi-registry.localhost:5001'

config.define_string('overlay')
cfg = config.parse()
overlay = cfg.get('overlay', 'mockauth')
if overlay not in ['local', 'mockauth', 'smoke']:
    fail("overlay '%s' is not available: use 'local', 'mockauth' or 'smoke'" % overlay)

# kiwi checks its sign-in provider at startup, so it waits for the mock; under smoke
# it also waits for the replay server its model calls go to.
auth_deps = ['mockoidc'] if overlay in ['mockauth', 'smoke'] else []
llm_deps = ['llmreplay'] if overlay == 'smoke' else []

# Everything here is meant for the local cluster and nothing else.
if k8s_context() != KUBE_CONTEXT:
    fail("kubectl context is '%s', expected '%s'; run `task local:up`" % (k8s_context(), KUBE_CONTEXT))

objects = kustomize('deploy/k8s/overlays/' + overlay)
k8s_yaml(objects)

# kiwi: cross-compile on the host, for the architecture of the host's Docker (the cluster
# runs on it), and wrap the binary in a one-layer image. The runtime image is distroless (no
# shell, no tar), so Tilt's live_update cannot sync into it; a rebuild is a compile and a
# COPY. The embedded migrations are part of the binary, so a change under
# backend/db/migrations rebuilds this image, which re-creates the migrate Job (same image)
# and so applies them.
custom_build(
    REGISTRY + '/kiwi',
    command=' && '.join([
        'out=$(mktemp -d)',
        '(cd backend && GOOS=linux GOARCH=$(go env GOARCH) CGO_ENABLED=0 go build -trimpath -o "$out/kiwi" ./cmd/kiwi)',
        'docker build -q -t $EXPECTED_REF -f deploy/local/kiwi.Dockerfile "$out"',
        'rm -rf "$out"',
    ]),
    deps=[
        'backend/cmd',
        'backend/internal',
        'backend/db/migrations',
        'backend/gen',
        'backend/go.mod',
        'backend/go.sum',
        'deploy/local/kiwi.Dockerfile',
    ],
    ignore=['**/*_test.go', 'backend/**/testdata'],
)

if overlay in ['mockauth', 'smoke']:
    # mockoidc: the mock sign-in provider, built from the `tools` stage of
    # backend/Dockerfile (a Go build inside Docker, cached by layer). It links only
    # the fakeoidc package, so only a change there or under tools/ rebuilds it.
    custom_build(
        REGISTRY + '/mockoidc',
        command='docker build -q --target tools -t $EXPECTED_REF backend',
        deps=[
            'backend/tools',
            'backend/internal/testkit/fakeoidc',
            'backend/Dockerfile',
            'backend/go.mod',
            'backend/go.sum',
        ],
        ignore=['**/*_test.go', 'backend/**/testdata', 'backend/internal/testkit/fakeoidc/fakeoidctest'],
    )
    k8s_resource(
        'mockoidc',
        objects=['mockoidc:ingress'],
        resource_deps=['kiwi-config'],
        links=[link('http://localhost:8080/mockoidc/.well-known/openid-configuration', 'mock sign-in')],
        labels=['deps'],
    )

if overlay == 'smoke':
    # llmreplay: the recorded LLM, the same `tools` stage as mockoidc under another
    # tag (the Deployment names /app/llmreplay). It imports nothing of K.I.W.I.'s, so
    # only a change under tools/llmreplay rebuilds it. A fixture is not part of the
    # image: the files of deploy/k8s/overlays/smoke/fixtures reach the pod as the
    # ConfigMap llmreplay-fixtures, whose name changes with its content, so editing
    # one rolls the replay out by itself.
    custom_build(
        REGISTRY + '/llmreplay',
        command='docker build -q --target tools -t $EXPECTED_REF backend',
        deps=[
            'backend/tools/llmreplay',
            'backend/Dockerfile',
            'backend/go.mod',
            'backend/go.sum',
        ],
        ignore=['**/*_test.go', 'backend/**/testdata'],
    )
    k8s_resource(
        'llmreplay',
        labels=['deps'],
    )

k8s_resource(
    'postgres',
    objects=['postgres-data:persistentvolumeclaim', 'postgres-init:configmap'],
    labels=['deps'],
)

k8s_resource(
    'valkey',
    labels=['deps'],
)

k8s_resource(
    'ollama',
    objects=['ollama-data:persistentvolumeclaim'],
    labels=['deps'],
)

# The ConfigMap kiwi-config has a resource of its own and no dependencies: mockoidc and kiwi
# both read it, kiwi waits for mockoidc, and Tilt applies an object only after the resource
# that owns it is allowed to start.
k8s_resource(
    new_name='kiwi-config',
    objects=['kiwi-config:configmap'],
    labels=['deps'],
)

k8s_resource(
    'ollama-pull',
    resource_deps=['ollama'],
    labels=['deps'],
)

k8s_resource(
    'migrate',
    resource_deps=['postgres'],
    labels=['app'],
)

k8s_resource(
    'kiwi',
    objects=['kiwi-data:persistentvolumeclaim', 'kiwi:ingress'],
    resource_deps=['kiwi-config', 'migrate', 'ollama-pull', 'valkey'] + auth_deps + llm_deps,
    links=[link('http://localhost:8080', 'K.I.W.I.')],
    labels=['app'],
)

# The Vite build runs inside the image build (apps/web/Dockerfile), so a change under
# apps/web or packages rebuilds the image. There is no live_update: the container runs as
# an unprivileged user with a read-only root filesystem, so nothing can be synced into
# it; the rebuild is the path.
docker_build(
    REGISTRY + '/web',
    '.',
    dockerfile='apps/web/Dockerfile',
    only=['apps/web', 'packages', 'package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'turbo.json'],
    ignore=['**/node_modules', '**/dist'],
)
k8s_resource(
    'web',
    labels=['app'],
)
