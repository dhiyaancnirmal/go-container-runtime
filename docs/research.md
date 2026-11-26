# Research: fast-start sandboxes for AI agents

Short survey (2026-11-26) to pick this project's angle. Vendor numbers are vendor claims unless marked otherwise; most products publish no p50/p99.

## What exists

| System | Isolation | Reported start | How it gets fast | Source |
|---|---|---|---|---|
| Firecracker | microVM (KVM, own guest kernel) | Cold boot < 125 ms to guest init; up to 150 microVMs/s/host | Minimal device model, minimal kernel and rootfs | [Firecracker NSDI'20 paper](https://css.csail.mit.edu/6.566/2023/readings/firecracker.pdf), [jvns.ca](https://jvns.ca/blog/2021/01/23/firecracker--start-a-vm-in-less-than-a-second/) |
| Firecracker snapshot restore | microVM | ~28 ms restore vs ~1.1 s cold boot (one developer's desktop, not official) | Skip kernel boot and init: restore memory image | [dev.to write-up](https://dev.to/adwitiya/how-i-built-sandboxes-that-boot-in-28ms-using-firecracker-snapshots-i0k) |
| E2B | Firecracker microVM per sandbox, own cgroup and netns | Third-party benchmark: 717 ms create, 662 ms resume; E2B docs: ~1 s resume | Boot from snapshots; "fork" a live sandbox into many | [LogRocket comparison](https://blog.logrocket.com/comparing-ai-agent-sandbox-platforms-e2b-modal-daytona-and-more/), [E2B infra README](https://raw.githubusercontent.com/e2b-dev/infra/main/README.md), [Fly.io comparison](https://fly.io/learn/sprites-vs-e2b-vs-modal-vs-daytona/) |
| Daytona | Containers | "Sub-90 ms" creation to first command (vendor claim) | Pre-allocated / pre-warmed pool | [daytona.io](https://daytona.io/), [docs](https://www.daytona.io/docs), [DEV comparison](https://dev.to/sahil_kat/daytona-vs-agentbox-vs-diy-sandbox-runtime-for-ai-agents-1a2i) |
| Modal Sandboxes | gVisor (user-space kernel) by default | gVisor boots a sandbox in ~25 ms with ~12 MB overhead (Modal engineering post); LogRocket third-party benchmark: 2347 ms snapshot boot | gVisor checkpoint/restore, lazy state loading | [Modal gVisor post](https://modal.com/blog/modal-gvisor-maintainer), [Modal memory snapshots](https://modal.com/blog/mem-snapshots), [sandbox snapshots docs](https://modal.com/docs/guide/sandbox-snapshots.md), [LogRocket comparison](https://blog.logrocket.com/comparing-ai-agent-sandbox-platforms-e2b-modal-daytona-and-more/) |
| CRIU | Checkpoint/restore any Linux process tree | Checkpoint time grows ~linearly with memory; restore grows less | Skip app init by restoring a warmed process | [Tošić, arXiv:2307.12113](https://arxiv.org/pdf/2307.12113), [Suneja et al., IWOC'17](https://www.cs.toronto.edu/~sahil/suneja-iwoc17.pdf) |
| FlashCube (research) | Containers | < 10 ms provision vs ~400 ms Docker (preliminary) | Assemble containers from pre-created parts: namespaces, cgroups, rootfs | [FlashCube paper](https://par.nsf.gov/servlets/purl/10358344), [project page](https://5ujinkang.github.io/flashcube-serverless/) |

## Where container start time goes

- Docker startup tracks runtime overhead (daemon, containerd, shim, runc) more than image size; namespace creation is only 8-10 ms (< 1.5%) of total. ([Khan, arXiv:2602.15214](https://arxiv.org/abs/2602.15214), preprint)
- A few operations dominate cold start, and they are OS mechanisms containers are built on. ([Georgia Tech, ACM SEC'22](https://faculty.cc.gatech.edu/~rama/pubs/ACM-SEC-2022-Cold-Start.pdf))
- Network namespaces are the known slow one, mostly on teardown (`cleanup_net`, RCU waits, netfilter). ([LKML: cleanup_net is slow](https://lkml.org/lkml/2017/4/21/665))
- crun's author found IPC and network namespace create/destroy costs when tuning crun startup. ([Red Hat: speed OCI containers](https://www.redhat.com/en/blog/speed-oci-containers))

## Pattern across products

Every fast product avoids doing the expensive work on the request path: snapshots (Firecracker, E2B, Modal/gVisor, CRIU) or pre-built pools (Daytona, FlashCube). Cold creation is the floor they all try to hide.

## Angle for this project

A microVM or gVisor build is out of scope for two days. A plain-Linux container runtime can still measure the question every agent-sandbox product answers:

1. **Cold start breakdown.** Time each phase of `run` (cgroup create, overlay mount, clone with namespaces, pivot_root and mounts, exec) to show where the time goes on a real kernel, including the net namespace cost.
2. **Warm pool.** FlashCube-style: pre-create the cgroup, overlay rootfs and namespaces with a parked init process; on request, hand it the command. Measure request-to-exit latency.
3. **Comparison.** Same command (`/bin/true` in an Alpine rootfs) through `docker run --rm` and `runc run` on the same host.

Headline: request-to-exit latency (p50/p99) for cold start, warm start, runc and Docker, plus the per-phase breakdown. This is honest (all measured on Doji), comparable to well-known tools, and explainable in an interview.
