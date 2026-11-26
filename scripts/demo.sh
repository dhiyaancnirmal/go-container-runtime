#!/bin/sh
# Record a demo transcript: every command shown is the command that ran.
# Usage: sudo scripts/demo.sh > results/demo.txt 2>&1   (needs bin/gcr and rootfs)
set -u
cd "$(dirname "$0")/.."
export PATH="$PWD/bin:$PATH" GCR_ROOTFS="$PWD/rootfs"

show() { printf '$ %s\n' "$*"; sh -c "$*"; }

show 'gcr run --memory 64m --pids 32 --hostname sandbox -- sh -c '\''echo "pid $$ on $(hostname)"; ls /sys/class/net; cat /etc/alpine-release; mount | head -1'\'''
echo
show 'gcr run --memory 32m -- tail /dev/zero; echo "exit $?"'
echo
printf '$ gcr serve --pool 4 &\n'
gcr serve --pool 4 &
server=$!
while [ ! -S /run/gcr.sock ]; do sleep 0.05; done
show 'gcr exec -- sh -c '\''echo hello from $(hostname); echo scratch > /tmp/note'\'''
show 'gcr exec -- sh -c '\''echo hello from $(hostname); cat /tmp/note 2>/dev/null || echo "no /tmp/note: fresh container"'\'''
printf '$ kill %%1\n'
kill "$server"
wait "$server"
show 'echo "containers left: $(ls /run/gcr | wc -l) state dirs, $(find /sys/fs/cgroup/gcr -mindepth 1 -type d | wc -l) cgroups"'
