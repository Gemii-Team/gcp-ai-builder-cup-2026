#!/usr/bin/env bash
# Creates the k3d cluster "kiwi" from deploy/local/k3d.yaml (starts it when it
# exists but is stopped) and selects its kubectl context.
#
# KIWI_K3D_CPUSET_HOOK=auto|1|0 (default auto) decides whether the nodes get
# deploy/local/k3d-entrypoint-cpuset.sh: auto adds it for rootless Docker whose
# user manager does not delegate the cpuset cgroup controller (k3s refuses to
# start there), 1 always adds it, 0 never. It matters only when the cluster is
# created; the decision is printed.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
CLUSTER=kiwi
mode="${KIWI_K3D_CPUSET_HOOK:-auto}"

# KIWI_K3D_USER_CONTROLLERS names the file that lists the controllers delegated
# to the user manager; only the tests change it.
user_controllers() {
  echo "${KIWI_K3D_USER_CONTROLLERS:-/sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/cgroup.controllers}"
}

rootless_without_cpuset() {
  # Captured first: `docker info | grep -q` exits at the match, and the writer's SIGPIPE
  # (141, a fail under pipefail) would then read as "not rootless".
  local info file
  info="$(docker info 2>/dev/null)" || return 1
  grep -qi rootless <<<"$info" || return 1
  file="$(user_controllers)"
  [ -r "$file" ] && ! grep -qw cpuset "$file"
}

# warn_if_app_port_is_public: the app port (80 of the load balancer, 8080 on the host) is
# published on loopback by deploy/local/k3d.yaml, but a cluster keeps the binding it was
# created with. An empty host IP is every interface. Read-only and advisory: a load
# balancer that cannot be inspected is not an error.
warn_if_app_port_is_public() {
  local bindings ip port public=()
  bindings="$(docker inspect --format '{{range (index .HostConfig.PortBindings "80/tcp")}}{{.HostIp}}|{{.HostPort}}{{"\n"}}{{end}}' "k3d-$CLUSTER-serverlb" 2>/dev/null)" || return 0
  while IFS='|' read -r ip port; do
    [ -n "$port" ] || continue
    case "$ip" in
      127.0.0.1 | ::1) ;;
      *) public+=("${ip:-all interfaces}:$port") ;;
    esac
  done <<<"$bindings"
  [ "${#public[@]}" -gt 0 ] || return 0
  echo "k3d-up: WARNING: the existing cluster publishes the app on ${public[*]}, so any host that can reach this machine can sign in and spend the model quota." >&2
  echo "k3d-up: WARNING: deploy/local/k3d.yaml now binds 127.0.0.1 only; the old binding stays until the cluster is recreated: task local:down, then task local:up (this deletes the cluster and its data)." >&2
}

case "$mode" in
  auto | 1 | 0) ;;
  *)
    echo "k3d-up: KIWI_K3D_CPUSET_HOOK must be auto, 1 or 0, got '$mode'" >&2
    exit 2
    ;;
esac

if k3d cluster get "$CLUSTER" >/dev/null 2>&1; then
  warn_if_app_port_is_public
  k3d cluster start "$CLUSTER"
else
  hook=off
  reason="KIWI_K3D_CPUSET_HOOK=0"
  case "$mode" in
    1)
      hook=on
      reason="KIWI_K3D_CPUSET_HOOK=1"
      ;;
    auto)
      if rootless_without_cpuset; then
        hook=on
        reason="rootless Docker whose user manager does not delegate cpuset"
      else
        reason="Docker is not rootless, or the user manager delegates cpuset"
      fi
      ;;
  esac
  echo "k3d-up: cpuset hook: $hook ($reason)"

  args=(--config deploy/local/k3d.yaml)
  if [ "$hook" = on ]; then
    args+=(
      --volume "$PWD/deploy/local/k3d-entrypoint-cpuset.sh:/bin/k3d-entrypoint-zz-cpuset.sh@server:*;agent:*"
      --k3s-arg '--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*;agent:*'
    )
  fi
  k3d cluster create "${args[@]}"
fi

kubectl config use-context "k3d-$CLUSTER" >/dev/null
