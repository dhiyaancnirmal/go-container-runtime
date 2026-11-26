# gcr: a Go container runtime built for fast sandbox start

`gcr` runs a command in an isolated Linux container (pid, mount, UTS, IPC and network namespaces, cgroup v2 limits, a copy-on-write overlay root) and removes every trace when it exits. It has a warm pool mode for AI-agent sandboxes: containers are built ahead of time and parked just before `exec`, so a request only pays for handing over the command.

The question it answers: how fast can a plain Linux container start, where does the time go, and how much does a pre-built pool save? Agent sandbox products (E2B, Daytona, Modal) all hide cold start with snapshots or pools; [docs/research.md](docs/research.md) surveys them and explains why this project measures a pool.

## A real run

From [results/demo.txt](results/demo.txt), on Doji (Debian 13, kernel 6.12):

```
$ sudo gcr run --memory 64m --pids 32 --hostname sandbox -- sh -c 'echo "pid $$ on $(hostname)"; ls /sys/class/net; cat /etc/alpine-release; mount | head -1'
pid 1 on sandbox
lo
3.24.4
overlay on / type overlay (rw,relatime,lowerdir=/home/dhiyaan/Code/go-container-runtime/rootfs,upperdir=/run/gcr/c770405e0589/upper,...)

$ sudo gcr run --memory 32m -- tail /dev/zero; echo "exit $?"
exit 137

$ sudo gcr serve --pool 4 &
$ sudo gcr exec -- sh -c 'echo hello from $(hostname)'
hello from 05c81c5b9884
$ sudo gcr exec -- sh -c 'echo hello from $(hostname)'
hello from 552200f35d2c
```

Each `exec` gets a fresh container (new hostname, fresh overlay); nothing written by one request is visible to the next.

## Results

Start-to-exit latency of `/bin/true` from Alpine 3.24 on one machine (AMD Ryzen 3 3200G, 4 cores, kernel 6.12, shared host, load 2.9 to 4.7 during the run). Every row is timed the same way: wall time of the harness running that CLI until it exits. n=200 per row. Source: [results/startup.json](results/startup.json) ([table](results/startup.md)).

| Runtime | p50 ms | p90 ms | p99 ms |
|---|---:|---:|---:|
| host fork+exec, no isolation (floor) | 0.65 | 0.76 | 2.27 |
| **gcr exec, warm pool** | **3.89** | **6.24** | **15.23** |
| gcr exec, pool drained | 6.43 | 9.38 | 21.34 |
| gcr run, cold | 5.87 | 8.28 | 20.60 |
| gcr run, cold, no network namespace | 5.44 | 7.14 | 14.52 |
| runc run 1.1.15 | 35.44 | 44.84 | 58.12 |
| docker run --network none (26.1.5) | 177.44 | 225.79 | 334.68 |
| docker run, bridge network | 276.65 | 344.30 | 433.00 |

What the numbers say:

- A warm-pool sandbox starts and finishes in 3.9 ms at the median: 1.5x faster than gcr's own cold path, 9x faster than runc and 46x faster than `docker run --network none`.
- The pool moves 4.13 ms (p50) of build work per container off the request path; each request pays for it in the background refill. When the pool is empty, a request costs 6.43 ms, about the same as a cold run.
- In the cold path, the largest step is `clone` into the new namespaces plus re-exec of the Go binary as init (2.41 ms of 5.87 ms). Overlay mount, the other mounts and `pivot_root` together take 0.68 ms; cgroup creation 0.12 ms; cleanup 0.18 ms.
- An empty network namespace adds about 0.4 ms at p50 to cold start. Its teardown runs asynchronously in the kernel, so no row includes it.
- Docker's extra time is its client, daemon, containerd and shim. Bridge networking adds about 100 ms more.

Caveats: runc's row uses runc's default spec with a read-only rootfs and no overlay, so runc does less filesystem work than gcr. The warm row includes the `gcr exec` client's own Go process start. The host is shared with other jobs, so p99 is noisy.

Cold `gcr run` phases, p50 ms (from `--timing`, separate runs):

| Step | p50 ms |
|---|---:|
| state dir and cgroup with limits | 0.28 |
| clone into namespaces and cgroup, re-exec as init | 2.41 |
| overlay and other mounts, pivot_root | 0.68 |
| loopback up, hostname | 0.11 |
| exec /bin/true, exit, PID namespace teardown, reap | 0.82 |
| cleanup (cgroup.kill, rmdir, remove overlay dirs) | 0.18 |

## Reproduce

Needs Linux with cgroup v2, overlayfs, root (sudo) and Go 1.24+. Docker and runc are optional comparison rows.

```sh
make test     # fetch Alpine rootfs (sha256-checked), build, run integration tests as root
make bench    # writes results/startup.json and results/startup.md
```

`make bench` runs every row 200 times after 10 warmups, interleaved round-robin so host noise hits each runtime alike, and records kernel, CPU and load average in the results file.

## How it works

`gcr run` (cold path), in [internal/container](internal/container):

1. **State and cgroup.** Make `/run/gcr/<id>/{upper,work,merged}` on tmpfs and a leaf cgroup `/sys/fs/cgroup/gcr/<id>` with `memory.max` (swap off, `memory.oom.group=1`), `pids.max` and `cpu.max`.
2. **Clone.** Re-execute the `gcr` binary as the container's init with `CLONE_NEWPID|NEWNS|NEWUTS|NEWIPC|NEWNET`, placed straight into the cgroup with `CLONE_INTO_CGROUP` (Go's `UseCgroupFD`), so there is no moment where it runs unlimited. Go cannot safely `unshare` a multithreaded process, which is why runc and gcr both re-exec. Config goes over a pipe.
3. **Root filesystem.** As PID 1 inside the new mount namespace: make `/` private, mount an overlay (lower = the shared read-only Alpine rootfs, upper = per-container tmpfs dir), mount `/proc`, read-only `/sys`, a tmpfs `/dev` with `null`, `zero`, `random` and friends, and `/tmp`; then `pivot_root` and detach the old root.
4. **Exec.** Bring up `lo`, set the hostname, `execve` the command. Each step writes a `CLOCK_MONOTONIC` timestamp to a pipe, which gives the phase breakdown.
5. **Cleanup.** When PID 1 exits the kernel kills the rest of the PID namespace and drops the mount namespace (and with it the overlay mount). gcr then writes `cgroup.kill`, removes the cgroup and deletes the state dir. On SIGINT/SIGTERM it kills the container first. If gcr itself is SIGKILLed, `Pdeathsig` kills the container and `gcr gc` removes the leftover directories.

`gcr serve` (warm path) does steps 1 to 3 ahead of time, then parks each init on a Unix socket. `gcr exec` sends the command plus its own stdin, stdout and stderr file descriptors (`SCM_RIGHTS`); the parked init `dup2`s them onto 0, 1, 2 and `exec`s. The daemon replies when the command exits, then tears that container down and builds a replacement in the background.

## Tests

[cmd/gcr/main_test.go](cmd/gcr/main_test.go) drives the real CLI against the kernel as root:

- each namespace is isolated (PID 1, own hostname, only `lo`, overlay root without the host's files, own IPC namespace);
- the overlay never writes to the shared rootfs and containers never see each other's writes;
- `--memory` OOM-kills (exit 137), `--pids 8` stops 20 forks that succeed without the limit, `--cpus 0.2` cuts a 1 s busy loop to about a fifth of the CPU time of an unlimited run;
- no cgroup, state dir or process survives a normal exit, a SIGTERM to the runtime, or a SIGKILL followed by `gcr gc`;
- warm pool requests get fresh containers and their exit codes and stdio.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs `make test` on every push.

## Limits

No user namespaces (runs as root, like rootful runc), no seccomp or capability dropping, no image pulling (a directory is the image), and network namespaces have only loopback. These are the next steps for a real multi-tenant sandbox; the benchmark compares against runc and Docker with networking matched where it matters (`--network none`).
