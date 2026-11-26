Start-to-exit latency of `/bin/true`, n=200 per row, 2026-11-26T20:16:32Z, kernel 6.12.111+deb13-amd64, 4 CPUs (AMD Ryzen 3 3200G with Radeon Vega Graphics). Load before/after: 2.14 3.62 4.39 3/1058 4169402 / 3.67 3.70 4.33 6/1119 38541.

| Runtime | p50 ms | p90 ms | p99 ms | min ms | failed |
|---|---:|---:|---:|---:|---:|
| host: fork+exec /bin/true (floor) | 0.66 | 0.80 | 3.88 | 0.49 | 0 |
| gcr exec, warm pool | 4.00 | 5.92 | 11.38 | 2.75 | 0 |
| gcr exec, pool drained (size 0) | 6.46 | 8.88 | 18.30 | 5.28 | 0 |
| gcr run, cold | 5.70 | 8.21 | 15.58 | 5.00 | 0 |
| gcr run, cold, host network | 5.29 | 7.47 | 13.39 | 4.52 | 0 |
| runc run | 35.54 | 44.97 | 51.41 | 21.00 | 0 |
| docker run --network none | 177.37 | 296.71 | 336.94 | 155.21 | 0 |
| docker run (bridge network) | 283.26 | 400.79 | 435.45 | 239.55 | 0 |

Cold `gcr run` phases (time between consecutive marks):

| Step | What happens | p50 ms | p90 ms |
|---|---|---:|---:|
| start → state_dir | create overlay upper/work/merged dirs | 0.133 | 0.165 |
| state_dir → cgroup | create cgroup, write limits | 0.114 | 0.133 |
| cgroup → init_start | clone into new namespaces and the cgroup, re-exec gcr as init | 2.188 | 2.737 |
| init_start → rootfs_ready | mount overlay, /proc, /sys, /dev, /tmp; pivot_root | 0.648 | 0.869 |
| rootfs_ready → namespaces_ready | bring up lo, set hostname | 0.102 | 0.146 |
| namespaces_ready → exec | resolve the command | 0.003 | 0.004 |
| exec → exited | execve /bin/true, run, exit, PID namespace teardown, reap | 0.736 | 1.502 |
| exited → cleaned_up | kill cgroup, remove it and the overlay dirs | 0.166 | 0.192 |

Warm `gcr exec` phases inside the daemon:

| Step | What happens | p50 ms | p90 ms |
|---|---|---:|---:|
| start → state_dir | create overlay upper/work/merged dirs | 0.075 | 0.224 |
| state_dir → cgroup | create cgroup, write limits | 0.119 | 0.169 |
| cgroup → init_start | clone into new namespaces and the cgroup, re-exec gcr as init | 2.565 | 3.846 |
| init_start → rootfs_ready | mount overlay, /proc, /sys, /dev, /tmp; pivot_root | 0.779 | 1.419 |
| rootfs_ready → namespaces_ready | bring up lo, set hostname | 0.146 | 0.368 |
| namespaces_ready → request_sent | idle in the pool (not on the request path) | 17.015 | 23.051 |
| request_sent → request_received | send command and stdio fds over the control socket | 0.105 | 0.507 |
| request_received → exec | resolve the command | 0.008 | 0.023 |
| exec → exited | execve /bin/true, run, exit, PID namespace teardown, reap | 1.041 | 2.972 |

Warm pool refill cost per container (off the request path): p50 4.11 ms, p90 5.91 ms.
