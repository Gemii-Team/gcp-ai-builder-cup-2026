#!/bin/sh
# Workaround for rootless Docker; scripts/local/k3d-up.sh mounts this file into
# every k3d node when it detects that case (KIWI_K3D_CPUSET_HOOK=auto, the
# default), and always or never when set to 1 or 0.
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
# overlay, then reads the real file. If containerd never shows up within 30 s
# the overlay is removed anyway and the reason lands in the k3d entrypoint log
# (/var/log/k3d-entrypoints_*.log in the node).
set -eu
if grep -qw cpuset /sys/fs/cgroup/cgroup.controllers; then
  exit 0
fi
printf '%s cpuset\n' "$(cat /sys/fs/cgroup/cgroup.controllers)" >/tmp/cgroup.controllers
mount --bind /tmp/cgroup.controllers /sys/fs/cgroup/cgroup.controllers
(
  i=0
  until [ -S /run/k3s/containerd/containerd.sock ]; do
    i=$((i + 1))
    if [ "$i" -gt 600 ]; then
      echo "[cpuset hook] containerd.sock did not appear within 30 s; removing the overlay anyway"
      break
    fi
    sleep 0.05
  done
  umount /sys/fs/cgroup/cgroup.controllers
) &
