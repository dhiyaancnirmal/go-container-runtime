Start-to-exit latency of `/bin/true`, n=200 per row, 2026-11-26T20:34:31Z, kernel 6.12.111+deb13-amd64, 4 CPUs (AMD Ryzen 3 3200G with Radeon Vega Graphics). Load before/after: 1.38 1.22 2.23 7/942 53361 / 3.84 2.17 2.46 4/920 115411.

| Runtime | p50 ms | p90 ms | p99 ms | min ms | failed |
|---|---:|---:|---:|---:|---:|
| host: fork+exec /bin/true (floor) | 0.66 | 0.75 | 1.07 | 0.50 | 0 |
| gcr exec, warm pool | 3.91 | 5.46 | 10.25 | 2.94 | 0 |
| gcr exec, pool drained (size 0) | 6.26 | 8.73 | 11.33 | 5.41 | 0 |
| gcr run, cold | 5.59 | 7.17 | 13.38 | 5.04 | 0 |
| gcr run, cold, host network | 5.17 | 6.54 | 11.99 | 4.60 | 0 |
| runc run | 34.66 | 42.59 | 49.61 | 19.10 | 0 |
| docker run --network none | 177.04 | 283.23 | 309.94 | 155.13 | 0 |
| docker run (bridge network) | 278.33 | 381.08 | 410.15 | 243.17 | 0 |

Cold `gcr run` phases (time between consecutive marks):

| Step | What happens | p50 ms | p90 ms |
|---|---|---:|---:|
| start → state_dir | create overlay upper/work/merged dirs | 0.127 | 0.153 |
| state_dir → cgroup | create cgroup, write limits | 0.106 | 0.122 |
| cgroup → init_start | clone into new namespaces and the cgroup, re-exec gcr as init | 1.928 | 2.208 |
| init_start → rootfs_ready | mount overlay, /proc, /sys, /dev, /tmp; pivot_root | 0.571 | 0.705 |
| rootfs_ready → namespaces_ready | bring up lo, set hostname | 0.092 | 0.110 |
| namespaces_ready → exec | resolve the command | 0.002 | 0.003 |
| exec → exited | execve /bin/true, run, exit, PID namespace teardown, reap | 0.698 | 0.892 |
| exited → cleaned_up | kill cgroup, remove it and the overlay dirs | 0.148 | 0.188 |

Warm `gcr exec` phases inside the daemon:

| Step | What happens | p50 ms | p90 ms |
|---|---|---:|---:|
| start → state_dir | create overlay upper/work/merged dirs | 0.073 | 0.160 |
| state_dir → cgroup | create cgroup, write limits | 0.115 | 0.159 |
| cgroup → init_start | clone into new namespaces and the cgroup, re-exec gcr as init | 2.181 | 2.759 |
| init_start → rootfs_ready | mount overlay, /proc, /sys, /dev, /tmp; pivot_root | 0.681 | 1.169 |
| rootfs_ready → namespaces_ready | bring up lo, set hostname | 0.127 | 0.174 |
| namespaces_ready → request_sent | idle in the pool (not on the request path) | 12.709 | 14.798 |
| request_sent → request_received | send command and stdio fds over the control socket | 0.092 | 0.265 |
| request_received → exec | resolve the command | 0.007 | 0.019 |
| exec → exited | execve /bin/true, run, exit, PID namespace teardown, reap | 0.868 | 1.479 |

Warm pool refill cost per container (off the request path): p50 3.39 ms, p90 4.40 ms.
