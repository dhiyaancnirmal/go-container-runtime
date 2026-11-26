# gcr: a Go container runtime built for fast sandbox start

`gcr` runs a command in an isolated Linux container (pid, mount, UTS, IPC and network namespaces, cgroup v2 limits, a copy-on-write overlay root) and removes every trace when it exits. It has a warm pool mode for AI-agent sandboxes: containers are built ahead of time and parked just before `exec`, so a request only pays for handing over the command.

The question it answers: how fast can a plain Linux container start, where does the time go, and how much does a pre-built pool save? Agent sandbox products (E2B, Daytona, Modal) all hide cold start with snapshots or pools; [docs/research.md](docs/research.md) surveys them and explains why this project measures a pool.

## A real run

From [results/demo.txt](results/demo.txt), recorded by [scripts/demo.sh](scripts/demo.sh) as root on Debian 13, kernel 6.12:

```
$ gcr run --memory 64m --pids 32 --hostname sandbox -- sh -c 'echo "pid $$ on $(hostname)"; ls /sys/class/net; cat /etc/alpine-release; mount | head -1'
pid 1 on sandbox
lo
3.24.4
overlay on / type overlay (rw,relatime,lowerdir=./rootfs,upperdir=/run/gcr/c983154ddda4/upper,workdir=/run/gcr/c983154ddda4/work,uuid=on)

$ gcr run --memory 32m -- tail /dev/zero; echo "exit $?"
exit 137

$ gcr serve --pool 4 &
$ gcr exec -- sh -c 'echo hello from $(hostname); echo scratch > /tmp/note'
hello from a1fdd2f7969b
$ gcr exec -- sh -c 'echo hello from $(hostname); cat /tmp/note 2>/dev/null || echo "no /tmp/note: fresh container"'
hello from 3acd340d0683
no /tmp/note: fresh container
$ kill %1
$ echo "containers left: $(ls /run/gcr | wc -l) state dirs, $(find /sys/fs/cgroup/gcr -mindepth 1 -type d | wc -l) cgroups"
containers left: 0 state dirs, 0 cgroups
```

Each `exec` gets a fresh container (new hostname, fresh overlay); nothing written by one request is visible to the next.

## Results

Start-to-exit latency of `/bin/true` from Alpine 3.24 on one machine (AMD Ryzen 3 3200G, 4 cores, kernel 6.12, shared with other jobs, 1-minute load 1.4 before and 3.8 after). Every row is timed the same way: wall time of the harness running that CLI until it exits. n=200 per row, interleaved round-robin. Source: [results/startup.json](results/startup.json) ([table](results/startup.md)).

| Runtime | p50 ms | p90 ms | p99 ms |
|---|---:|---:|---:|
| host fork+exec, no isolation (floor) | 0.66 | 0.75 | 1.07 |
| **gcr exec, warm pool** | **3.91** | **5.46** | **10.25** |
| gcr exec, pool drained | 6.26 | 8.73 | 11.33 |
| **gcr run, cold** | **5.59** | **7.17** | **13.38** |
| gcr run, cold, no network namespace | 5.17 | 6.54 | 11.99 |
| runc run 1.1.15 | 34.66 | 42.59 | 49.61 |
| docker run --network none (26.1.5) | 177.04 | 283.23 | 309.94 |
| docker run, bridge network | 278.33 | 381.08 | 410.15 |

What the numbers say:

- **Cold against cold**, gcr starts and exits in 5.59 ms at the median: 6.2x faster than `runc run` (34.66 ms) and 32x faster than `docker run --network none` (177.04 ms).
- **The warm pool** brings that to 3.91 ms, 1.4x faster than gcr's cold path. It moves 3.39 ms (p50) of build work per container off the request path into the background refill. With the pool empty, a request costs 6.26 ms, about the same as a cold run.
- **Why the pool saves only 1.4x:** inside the daemon, a warm request takes about 0.97 ms from handing over the command to exit (sum of p50 steps below), against about 3.5 ms for a cold container's start to exit. The other ~2.9 ms of the warm row is the `gcr exec` client: starting a Go process, connecting and the socket round trip. An in-process client would get close to that 0.97 ms; this is the next thing to optimise.
- **Where cold time goes:** the largest step is `clone` into the new namespaces plus re-exec of the Go binary as init (1.93 ms). Overlay and the other mounts plus `pivot_root` take 0.57 ms, cgroup creation 0.11 ms, cleanup 0.15 ms.
- **Network namespaces:** an empty one adds about 0.4 ms at p50. Its teardown runs asynchronously in the kernel, so no row includes it. Bridge networking adds about 101 ms to `docker run`.

What the comparison does and does not show:

- gcr does less than runc. runc's default spec also drops capabilities, masks and read-only-mounts parts of `/proc` and `/sys`, sets up more mounts, applies the full OCI config and writes state files; gcr does none of that. In return, runc's row uses a read-only rootfs with no overlay, so gcr does more filesystem work. Read the runc and Docker rows as "what a full-featured runtime costs on this machine", not as a like-for-like race.
- Docker's extra ~140 ms over runc is most likely its client, dockerd, containerd and shim layers ([research notes](docs/research.md#where-container-start-time-goes)); this benchmark does not break that down.
- The host was shared with other jobs, so p99 is noisy.

Cold `gcr run` steps, p50 ms (from `--timing`, separate runs):

| Step | p50 ms |
|---|---:|
| state dir, cgroup and limits | 0.23 |
| clone into namespaces and cgroup, re-exec as init | 1.93 |
| overlay and other mounts, pivot_root | 0.57 |
| loopback up, hostname | 0.09 |
| exec /bin/true, exit, PID namespace teardown, reap | 0.70 |
| cleanup (cgroup.kill, rmdir, remove overlay dirs) | 0.15 |

Warm `gcr exec` steps on the request path, p50 ms:

| Step | p50 ms |
|---|---:|
| send command and stdio fds to the parked init | 0.09 |
| resolve the command | 0.01 |
| exec /bin/true, exit, PID namespace teardown, reap | 0.87 |

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
- warm pool requests get fresh containers and their exit codes and stdio;
- stopping `gcr serve` mid-request kills the running and idle containers and leaves nothing behind.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs `make test` on every push.

## Limits

gcr isolates and limits processes, but it is not yet a security boundary for untrusted code: no user namespaces (runs as root, like rootful runc), no seccomp or capability dropping, no image pulling (a directory is the image), and network namespaces have only loopback. These are the next steps for a real multi-tenant sandbox; the benchmark compares against runc and Docker with networking matched where it matters (`--network none`).
