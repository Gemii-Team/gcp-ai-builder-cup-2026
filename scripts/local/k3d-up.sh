#!/usr/bin/env bash
# Creates the k3d cluster "kiwi" from deploy/local/k3d.yaml (starts it when it
# exists but is stopped) and selects its kubectl context.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
CLUSTER=kiwi

# Rootless Docker only passes on the cgroup v2 controllers its user manager
# delegates, and by default that excludes cpuset, which k3s demands at start
# (see deploy/local/k3d-entrypoint-cpuset.sh).
rootless_without_cpuset() {
  docker info 2>/dev/null | grep -qi rootless || return 1
  local user_cgroup
  user_cgroup="/sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/cgroup.controllers"
  [ -r "$user_cgroup" ] && ! grep -qw cpuset "$user_cgroup"
}

if k3d cluster get "$CLUSTER" >/dev/null 2>&1; then
  k3d cluster start "$CLUSTER"
else
  args=(--config deploy/local/k3d.yaml)
  if rootless_without_cpuset; then
    echo "k3d-up: rootless Docker without the cpuset controller; adding the workaround from deploy/local/k3d-entrypoint-cpuset.sh"
    args+=(
      --volume "$PWD/deploy/local/k3d-entrypoint-cpuset.sh:/bin/k3d-entrypoint-zz-cpuset.sh@server:*;agent:*"
      --k3s-arg '--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*;agent:*'
    )
  fi
  k3d cluster create "${args[@]}"
fi

kubectl config use-context "k3d-$CLUSTER" >/dev/null
