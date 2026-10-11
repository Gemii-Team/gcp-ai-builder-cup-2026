#!/bin/sh
# Opt-in workaround for rootless Docker; see `task local:up` (K3D_ARGS).
#
# k3d runs every /bin/k3d-entrypoint-*.sh before it starts k3s. Rootless Docker
# gives a container only the cgroup v2 controllers its user manager delegates,
# and systemd delegates "cpu io memory pids" by default. k3s refuses to start
# without cpuset in that list ("failed to find cpuset cgroup (v2)") although
# nothing here pins CPUs.
#
# The fix is to delegate cpuset (a Delegate= drop-in for user@.service, which
# needs root). Without it, this hook lists cpuset for k3s's startup check only:
# the list is overlaid on cgroup.controllers until containerd, started after
# that check, is listening. The kubelet, which starts later and rejects the
# overlay, then reads the real file.
set -eu
if grep -qw cpuset /sys/fs/cgroup/cgroup.controllers; then
  exit 0
fi
printf '%s cpuset\n' "$(cat /sys/fs/cgroup/cgroup.controllers)" >/tmp/cgroup.controllers
mount --bind /tmp/cgroup.controllers /sys/fs/cgroup/cgroup.controllers
(
  until [ -S /run/k3s/containerd/containerd.sock ]; do sleep 0.05; done
  umount /sys/fs/cgroup/cgroup.controllers
) &
