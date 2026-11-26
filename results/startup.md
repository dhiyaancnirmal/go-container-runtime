Start-to-exit latency of `/bin/true`, n=200 per row, 2026-11-26T20:09:21Z, kernel 6.12.111+deb13-amd64, 4 CPUs (AMD Ryzen 3 3200G with Radeon Vega Graphics). Load before/after: 2.85 5.36 5.01 3/1153 4100223 / 4.73 5.24 5.00 7/1197 4162363.

| Runtime | p50 ms | p90 ms | p99 ms | min ms | failed |
|---|---:|---:|---:|---:|---:|
| host: fork+exec /bin/true (floor) | 0.65 | 0.76 | 2.27 | 0.51 | 0 |
| gcr exec, warm pool | 3.89 | 6.24 | 15.23 | 2.95 | 0 |
| gcr exec, pool drained (size 0) | 6.43 | 9.38 | 21.34 | 5.46 | 0 |
| gcr run, cold | 5.87 | 8.28 | 20.60 | 5.12 | 0 |
| gcr run, cold, host network | 5.44 | 7.14 | 14.52 | 4.62 | 0 |
| runc run | 35.44 | 44.84 | 58.12 | 20.18 | 0 |
| docker run --network none | 177.44 | 225.79 | 334.68 | 156.68 | 0 |
| docker run (bridge network) | 276.65 | 344.30 | 433.00 | 240.32 | 0 |

Cold `gcr run` phases (time between consecutive marks):

| Step | What happens | p50 ms | p90 ms |
|---|---|---:|---:|
| start → state_dir | create overlay upper/work/merged dirs | 0.153 | 0.181 |
| state_dir → cgroup | create cgroup, write limits | 0.123 | 0.143 |
| cgroup → init_start | clone into new namespaces and the cgroup, re-exec gcr as init | 2.414 | 3.706 |
| init_start → rootfs_ready | mount overlay, /proc, /sys, /dev, /tmp; pivot_root | 0.684 | 1.048 |
| rootfs_ready → namespaces_ready | bring up lo, set hostname | 0.110 | 0.157 |
| namespaces_ready → exec | resolve the command | 0.003 | 0.005 |
| exec → exited | execve /bin/true, run, exit, PID namespace teardown, reap | 0.816 | 1.148 |
| exited → cleaned_up | kill cgroup, remove it and the overlay dirs | 0.178 | 0.209 |

Warm `gcr exec` phases inside the daemon:

| Step | What happens | p50 ms | p90 ms |
|---|---|---:|---:|
| start → state_dir | create overlay upper/work/merged dirs | 0.079 | 0.236 |
| state_dir → cgroup | create cgroup, write limits | 0.118 | 0.216 |
| cgroup → init_start | clone into new namespaces and the cgroup, re-exec gcr as init | 2.617 | 3.813 |
| init_start → rootfs_ready | mount overlay, /proc, /sys, /dev, /tmp; pivot_root | 0.770 | 1.381 |
| rootfs_ready → namespaces_ready | bring up lo, set hostname | 0.146 | 0.335 |
| namespaces_ready → request_sent | idle in the pool (not on the request path) | 18.792 | 25.512 |
| request_sent → request_received | send command and stdio fds over the control socket | 0.119 | 0.862 |
| request_received → exec | resolve the command | 0.008 | 0.022 |
| exec → exited | execve /bin/true, run, exit, PID namespace teardown, reap | 1.227 | 2.820 |

Warm pool refill cost per container (off the request path): p50 4.13 ms, p90 5.69 ms.
