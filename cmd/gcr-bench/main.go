// Command gcr-bench measures container start-to-exit latency for gcr (cold
// and warm), runc and Docker with one clock: the wall time of this harness
// running each CLI to completion. Runs are interleaved round-robin so host
// noise hits every runtime alike. Needs root.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

type benchCase struct {
	Name  string   `json:"name"`
	Argv  []string `json:"argv"`
	Clock string   `json:"clock"`
}

type stats struct {
	N      int     `json:"n"`
	Min    float64 `json:"min_ms"`
	P50    float64 `json:"p50_ms"`
	P90    float64 `json:"p90_ms"`
	P99    float64 `json:"p99_ms"`
	Mean   float64 `json:"mean_ms"`
	Max    float64 `json:"max_ms"`
	Failed int     `json:"failed"`
}

type caseResult struct {
	benchCase
	Stats stats `json:"stats"`
}

type phaseStat struct {
	From string  `json:"from"`
	To   string  `json:"to"`
	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
}

type report struct {
	Date       string            `json:"date"`
	Host       map[string]string `json:"host"`
	N          int               `json:"n_per_case"`
	Warmup     int               `json:"warmup_per_case"`
	Command    string            `json:"command"`
	Cases      []caseResult      `json:"cases"`
	ColdPhases []phaseStat       `json:"cold_phases"`
	WarmPhases []phaseStat       `json:"warm_phases"`
	WarmFill   stats             `json:"warm_fill"`
	LoadBefore string            `json:"loadavg_before"`
	LoadAfter  string            `json:"loadavg_after"`
	Notes      []string          `json:"notes"`
}

func main() {
	gcr := flag.String("gcr", "bin/gcr", "gcr binary")
	rootfs := flag.String("rootfs", "rootfs", "Alpine rootfs directory")
	image := flag.String("image", "alpine:3.24", "Docker image matching the rootfs")
	n := flag.Int("n", 200, "measured runs per case")
	warmup := flag.Int("warmup", 10, "unmeasured runs per case first")
	outDir := flag.String("out", "results", "output directory")
	flag.Parse()
	if os.Geteuid() != 0 {
		log.Fatal("run as root")
	}
	*gcr, _ = filepath.Abs(*gcr)
	*rootfs, _ = filepath.Abs(*rootfs)
	tmp, err := os.MkdirTemp("", "gcr-bench")
	check(err)
	defer os.RemoveAll(tmp)

	warmSock := filepath.Join(tmp, "warm.sock")
	drainedSock := filepath.Join(tmp, "drained.sock")
	stopWarm := serve(*gcr, *rootfs, warmSock, 4)
	defer stopWarm()
	stopDrained := serve(*gcr, *rootfs, drainedSock, 0)
	defer stopDrained()

	cases := []benchCase{
		{"host: fork+exec /bin/true (floor)", []string{"/bin/true"}, "harness fork+exec+wait, no isolation"},
		{"gcr exec, warm pool", []string{*gcr, "exec", "--socket", warmSock, "--", "/bin/true"}, "client CLI start to exit; pool already holds a ready container"},
		{"gcr exec, pool drained (size 0)", []string{*gcr, "exec", "--socket", drainedSock, "--", "/bin/true"}, "client CLI start to exit; container built on request through the daemon"},
		{"gcr run, cold", []string{*gcr, "run", "--rootfs", *rootfs, "--", "/bin/true"}, "CLI start to exit, includes cleanup"},
		{"gcr run, cold, host network", []string{*gcr, "run", "--host-net", "--rootfs", *rootfs, "--", "/bin/true"}, "same, without a network namespace"},
	}
	notes := []string{
		"Every row runs /bin/true from Alpine 3.24 and is timed the same way: wall time of the harness running the CLI until it exits.",
		"gcr and runc rows create pid, mount, uts, ipc and network namespaces (gcr --host-net skips network). Network namespaces contain only lo.",
		"Network namespace teardown in the kernel is asynchronous, so it is not in any row.",
		"The warm pool refills in the background after each request; warm_fill is that hidden cost per container.",
	}
	if p, err := exec.LookPath("runc"); err == nil {
		bundle := runcBundle(tmp, *rootfs)
		cases = append(cases, benchCase{"runc run", []string{p, "run", "--bundle", bundle, "ID"}, "CLI start to exit, default runc spec, read-only rootfs (no overlay)"})
	}
	if p, err := exec.LookPath("docker"); err == nil {
		if out, err := exec.Command(p, "pull", "-q", *image).CombinedOutput(); err != nil {
			log.Fatalf("docker pull: %v %s", err, out)
		}
		cases = append(cases,
			benchCase{"docker run --network none", []string{p, "run", "--rm", "--network", "none", *image, "/bin/true"}, "CLI start to exit (client, dockerd, containerd, shim, runc)"},
			benchCase{"docker run (bridge network)", []string{p, "run", "--rm", *image, "/bin/true"}, "same, default bridge network"},
		)
	}

	rep := report{
		Date: time.Now().UTC().Format(time.RFC3339), Host: hostInfo(), N: *n, Warmup: *warmup,
		Command: "/bin/true", Notes: notes, LoadBefore: loadavg(),
	}
	samples := make([][]float64, len(cases))
	failed := make([]int, len(cases))
	seq := 0
	for i := -*warmup; i < *n; i++ {
		for ci, c := range cases {
			seq++
			argv := append([]string(nil), c.Argv...)
			for k, a := range argv {
				if a == "ID" {
					argv[k] = fmt.Sprintf("bench-%d", seq)
				}
			}
			ms, err := timeRun(argv)
			if i < 0 {
				continue
			}
			if err != nil {
				failed[ci]++
				log.Printf("%s: %v", c.Name, err)
				continue
			}
			samples[ci] = append(samples[ci], ms)
		}
		if i >= 0 && (i+1)%50 == 0 {
			log.Printf("%d/%d rounds", i+1, *n)
		}
	}
	// Record argv with local paths shortened so the results file holds no
	// machine-specific directories.
	short := strings.NewReplacer(*gcr, "gcr", *rootfs, "rootfs", tmp, "$TMP")
	for ci, c := range cases {
		s := summarize(samples[ci])
		s.Failed = failed[ci]
		shown := c
		shown.Argv = nil
		for _, a := range c.Argv {
			shown.Argv = append(shown.Argv, short.Replace(a))
		}
		rep.Cases = append(rep.Cases, caseResult{shown, s})
	}

	// Phase breakdowns come from separate runs so writing timing files does
	// not touch the latency rows above.
	rep.ColdPhases = phases(*n, func(f string) []string {
		return []string{*gcr, "run", "--timing", f, "--rootfs", *rootfs, "--", "/bin/true"}
	}, tmp, nil)
	var fills []float64
	rep.WarmPhases = phases(*n, func(f string) []string {
		return []string{*gcr, "exec", "--timing", f, "--socket", warmSock, "--", "/bin/true"}
	}, tmp, &fills)
	rep.WarmFill = summarize(fills)
	rep.LoadAfter = loadavg()

	check(os.MkdirAll(*outDir, 0o755))
	b, _ := json.MarshalIndent(rep, "", "  ")
	check(os.WriteFile(filepath.Join(*outDir, "startup.json"), append(b, '\n'), 0o644))
	md := markdown(rep)
	check(os.WriteFile(filepath.Join(*outDir, "startup.md"), []byte(md), 0o644))
	fmt.Print(md)
}

func timeRun(argv []string) (float64, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	ms := float64(time.Since(start).Nanoseconds()) / 1e6
	if err != nil {
		return 0, fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return ms, nil
}

func serve(gcr, rootfs, sock string, size int) func() {
	cmd := exec.Command(gcr, "serve", "--rootfs", rootfs, "--socket", sock, "--pool", fmt.Sprint(size))
	cmd.Stderr = os.Stderr
	check(cmd.Start())
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Fatal("serve did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let the pool fill
	return func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	}
}

func runcBundle(tmp, rootfs string) string {
	dir := filepath.Join(tmp, "runc")
	check(os.MkdirAll(dir, 0o755))
	c := exec.Command("runc", "spec")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		log.Fatalf("runc spec: %v %s", err, out)
	}
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(path)
	check(err)
	var spec map[string]any
	check(json.Unmarshal(raw, &spec))
	spec["root"] = map[string]any{"path": rootfs, "readonly": true}
	proc := spec["process"].(map[string]any)
	proc["terminal"] = false
	proc["args"] = []string{"/bin/true"}
	out, _ := json.MarshalIndent(spec, "", "  ")
	check(os.WriteFile(path, out, 0o644))
	return dir
}

type timingFile struct {
	FillNS int64 `json:"fill_ns"`
	Phases []struct {
		Name string `json:"name"`
		NS   int64  `json:"ns"`
	} `json:"phases"`
}

// phases runs argv n times and reports the median time between consecutive phases.
func phases(n int, argv func(string) []string, tmp string, fills *[]float64) []phaseStat {
	f := filepath.Join(tmp, "timing.json")
	var order [][2]string
	deltas := map[[2]string][]float64{}
	for range n {
		os.Remove(f)
		if _, err := timeRun(argv(f)); err != nil {
			log.Printf("phase run: %v", err)
			continue
		}
		raw, err := os.ReadFile(f)
		check(err)
		var tf timingFile
		check(json.Unmarshal(raw, &tf))
		if fills != nil {
			*fills = append(*fills, float64(tf.FillNS)/1e6)
		}
		// "clone" is when the parent's fork call returned, which races with
		// the child's first mark; leave it out so the sequence is fixed.
		ps := tf.Phases[:0]
		for _, p := range tf.Phases {
			if p.Name != "clone" {
				ps = append(ps, p)
			}
		}
		for i := 1; i < len(ps); i++ {
			k := [2]string{ps[i-1].Name, ps[i].Name}
			if _, ok := deltas[k]; !ok {
				order = append(order, k)
			}
			deltas[k] = append(deltas[k], float64(ps[i].NS-ps[i-1].NS)/1e6)
		}
	}
	var out []phaseStat
	for _, k := range order {
		s := summarize(deltas[k])
		out = append(out, phaseStat{From: k[0], To: k[1], P50: s.P50, P90: s.P90})
	}
	return out
}

func summarize(xs []float64) stats {
	if len(xs) == 0 {
		return stats{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	q := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(s)))) - 1
		return round(s[max(0, min(i, len(s)-1))])
	}
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	return stats{N: len(s), Min: round(s[0]), P50: q(.5), P90: q(.9), P99: q(.99), Mean: round(sum / float64(len(s))), Max: round(s[len(s)-1])}
}

func round(x float64) float64 { return math.Round(x*1000) / 1000 }

func hostInfo() map[string]string {
	h := map[string]string{"go": runtime.Version(), "cpus": fmt.Sprint(runtime.NumCPU())}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h["kernel"] = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				h["cpu"] = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
				break
			}
		}
	}
	for name, argv := range map[string][]string{
		"runc":   {"runc", "--version"},
		"docker": {"docker", "version", "--format", "{{.Server.Version}}"},
	} {
		if out, err := exec.Command(argv[0], argv[1:]...).Output(); err == nil {
			h[name] = strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		}
	}
	return h
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	return strings.TrimSpace(string(b))
}

var phaseWhat = map[string]string{
	"start>state_dir":               "create overlay upper/work/merged dirs",
	"state_dir>cgroup":              "create cgroup, write limits",
	"cgroup>init_start":             "clone into new namespaces and the cgroup, re-exec gcr as init",
	"init_start>rootfs_ready":       "mount overlay, /proc, /sys, /dev, /tmp; pivot_root",
	"rootfs_ready>namespaces_ready": "bring up lo, set hostname",
	"namespaces_ready>exec":         "resolve the command",
	"namespaces_ready>request_sent": "idle in the pool (not on the request path)",
	"request_sent>request_received": "send command and stdio fds over the control socket",
	"request_received>exec":         "resolve the command",
	"exec>exited":                   "execve /bin/true, run, exit, PID namespace teardown, reap",
	"exited>cleaned_up":             "kill cgroup, remove it and the overlay dirs",
}

func markdown(r report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Start-to-exit latency of `/bin/true`, n=%d per row, %s, kernel %s, %s CPUs (%s). Load before/after: %s / %s.\n\n",
		r.N, r.Date, r.Host["kernel"], r.Host["cpus"], r.Host["cpu"], r.LoadBefore, r.LoadAfter)
	b.WriteString("| Runtime | p50 ms | p90 ms | p99 ms | min ms | failed |\n|---|---:|---:|---:|---:|---:|\n")
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "| %s | %.2f | %.2f | %.2f | %.2f | %d |\n", c.Name, c.Stats.P50, c.Stats.P90, c.Stats.P99, c.Stats.Min, c.Stats.Failed)
	}
	table := func(title string, ps []phaseStat) {
		fmt.Fprintf(&b, "\n%s\n\n| Step | What happens | p50 ms | p90 ms |\n|---|---|---:|---:|\n", title)
		for _, p := range ps {
			fmt.Fprintf(&b, "| %s → %s | %s | %.3f | %.3f |\n", p.From, p.To, phaseWhat[p.From+">"+p.To], p.P50, p.P90)
		}
	}
	table("Cold `gcr run` phases (time between consecutive marks):", r.ColdPhases)
	table("Warm `gcr exec` phases inside the daemon:", r.WarmPhases)
	fmt.Fprintf(&b, "\nWarm pool refill cost per container (off the request path): p50 %.2f ms, p90 %.2f ms.\n", r.WarmFill.P50, r.WarmFill.P90)
	return b.String()
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
