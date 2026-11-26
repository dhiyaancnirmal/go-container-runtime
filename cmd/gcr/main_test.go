package main

// Integration tests: they drive the real CLI (this test binary re-executed as
// gcr) against a real kernel, so they need root and an Alpine rootfs.
// Run with: make test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dhiyaancnirmal/go-container-runtime/internal/container"
)

const asCLI = "_GCR_TEST_AS_CLI"

var rootfs string

func TestMain(m *testing.M) {
	if container.IsInit() {
		container.InitMain()
		return
	}
	if os.Getenv(asCLI) == "1" {
		main()
		return
	}
	rootfs = os.Getenv("GCR_ROOTFS")
	if rootfs == "" {
		rootfs, _ = filepath.Abs("../../rootfs")
	}
	os.Exit(m.Run())
}

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (run: make test)")
	}
	if _, err := os.Stat(filepath.Join(rootfs, "etc/alpine-release")); err != nil {
		t.Skipf("no rootfs at %s (run: scripts/fetch-rootfs.sh)", rootfs)
	}
}

// gcr runs the CLI and returns stdout, stderr and the exit code.
func gcr(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := gcrCmd(args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("gcr %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

func gcrCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), asCLI+"=1", "GCR_ROOTFS="+rootfs)
	return cmd
}

func run(t *testing.T, flagsAndCmd ...string) (string, int) {
	t.Helper()
	out, errOut, code := gcr(t, append([]string{"run"}, flagsAndCmd...)...)
	if errOut != "" {
		t.Logf("stderr: %s", errOut)
	}
	return out, code
}

func TestIsolation(t *testing.T) {
	needRoot(t)
	hostName, _ := os.Hostname()
	cases := []struct {
		name  string
		args  []string
		check func(t *testing.T, out string)
	}{
		{"pid namespace: command is PID 1 and sees no host processes",
			[]string{"--", "sh", "-c", "echo $$; ls /proc | grep -c '^[0-9]'"},
			func(t *testing.T, out string) {
				// $$ is 1; the only numeric /proc entries are sh and its pipeline (<= 3).
				lines := strings.Fields(out)
				if n, _ := strconv.Atoi(lines[len(lines)-1]); len(lines) != 2 || lines[0] != "1" || n > 3 {
					t.Fatalf("want PID 1 and <=3 processes, got %q", out)
				}
			}},
		{"uts namespace: hostname is set and differs from host",
			[]string{"--hostname", "sandbox", "--", "hostname"},
			func(t *testing.T, out string) {
				if strings.TrimSpace(out) != "sandbox" || hostName == "sandbox" {
					t.Fatalf("hostname %q", out)
				}
			}},
		{"net namespace: only loopback, and it is up",
			[]string{"--", "sh", "-c", "ls /sys/class/net; cat /sys/class/net/lo/operstate /sys/class/net/lo/flags"},
			func(t *testing.T, out string) {
				f := strings.Fields(out)
				// lo's operstate is "unknown" once up; flags has IFF_UP (0x1).
				if len(f) != 3 || f[0] != "lo" || !strings.HasSuffix(f[2], "9") {
					t.Fatalf("want only lo, up; got %q", out)
				}
			}},
		{"mount namespace: root is the overlay of the Alpine rootfs, host files are absent",
			[]string{"--", "sh", "-c", "cat /etc/alpine-release; test -e /.oldroot || echo no-oldroot; mount | grep ' on / '"},
			func(t *testing.T, out string) {
				if !strings.HasPrefix(out, "3.") || !strings.Contains(out, "no-oldroot") || !strings.Contains(out, "overlay on / type overlay") {
					t.Fatalf("unexpected root: %q", out)
				}
			}},
		{"ipc namespace: has its own System V IPC ids",
			[]string{"--", "sh", "-c", "readlink /proc/1/ns/ipc"},
			func(t *testing.T, out string) {
				host, _ := os.Readlink("/proc/self/ns/ipc")
				if strings.TrimSpace(out) == host {
					t.Fatalf("ipc namespace is the host's: %s", out)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d, out %q", code, out)
			}
			tc.check(t, out)
		})
	}
}

func TestExitCodeAndStdioPassThrough(t *testing.T) {
	needRoot(t)
	cmd := gcrCmd("run", "--", "sh", "-c", "read x; echo got:$x; echo err >&2; exit 7")
	cmd.Stdin = strings.NewReader("hello\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Fatalf("want exit 7, got %v", err)
	}
	if out.String() != "got:hello\n" || errb.String() != "err\n" {
		t.Fatalf("stdout %q stderr %q", out.String(), errb.String())
	}
}

func TestOverlayKeepsRootfsAndContainersApart(t *testing.T) {
	needRoot(t)
	if _, code := run(t, "--", "sh", "-c", "echo x > /etc/leak && rm /bin/ls"); code != 0 {
		t.Fatalf("write failed: %d", code)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "etc/leak")); err == nil {
		t.Fatal("write reached the lower rootfs")
	}
	if _, err := os.Lstat(filepath.Join(rootfs, "bin/ls")); err != nil {
		t.Fatal("delete reached the lower rootfs")
	}
	out, _ := run(t, "--", "sh", "-c", "test -e /etc/leak && echo leaked; test -e /bin/ls && echo ls-present")
	if strings.Contains(out, "leaked") || !strings.Contains(out, "ls-present") {
		t.Fatalf("next container saw the previous one's changes: %q", out)
	}
}

func TestMemoryLimitKillsContainer(t *testing.T) {
	needRoot(t)
	// tail buffers /dev/zero forever, so it grows until the limit is hit.
	out, code := run(t, "--memory", "32m", "--", "tail", "/dev/zero")
	if code != 128+int(syscall.SIGKILL) {
		t.Fatalf("want OOM kill (137), got %d %q", code, out)
	}
}

func TestPidsLimitStopsForkBomb(t *testing.T) {
	needRoot(t)
	spawn := []string{"--", "sh", "-c", "for i in $(seq 20); do sleep 1 & done; echo spawned"}
	if out, code := run(t, spawn...); code != 0 || !strings.Contains(out, "spawned") {
		t.Fatalf("control without limit failed: %d %q", code, out)
	}
	out, errOut, code := gcr(t, append([]string{"run", "--pids", "8"}, spawn...)...)
	if code == 0 || strings.Contains(out, "spawned") || !strings.Contains(errOut, "can't fork") {
		t.Fatalf("pids.max=8 did not stop 20 forks: code %d out %q err %q", code, out, errOut)
	}
}

func TestCPULimitThrottles(t *testing.T) {
	needRoot(t)
	// Busy-loop for 1s of wall time, then print the children's CPU time in
	// clock ticks (100/s). Unlimited is ~100; with 0.2 CPUs it must be ~20.
	burn := []string{"--", "sh", "-c", `timeout 1 sh -c 'while :; do :; done' 2>/dev/null; awk '{print $16+$17}' /proc/$$/stat`}
	ticks := func(flags ...string) int {
		out, _ := run(t, append(flags, burn...)...)
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			t.Fatalf("bad output %q", out)
		}
		return n
	}
	free, limited := ticks(), ticks("--cpus", "0.2")
	if free < 60 || limited > 35 {
		t.Fatalf("want ~100 ticks unlimited and ~20 at 0.2 CPUs, got %d and %d", free, limited)
	}
}

func assertNoLeftovers(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		cgs, _ := filepath.Glob(filepath.Join(container.CgroupRoot, "*", "cgroup.procs"))
		states, _ := filepath.Glob(filepath.Join(container.StateRoot, "*"))
		if len(cgs) == 0 && len(states) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leftovers: cgroups %v, state %v", cgs, states)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCleanupAfterNormalExit(t *testing.T) {
	needRoot(t)
	run(t, "--", "sh", "-c", "sleep 0.1 & exit 3")
	assertNoLeftovers(t)
}

func TestCleanupWhenRuntimeIsSignalled(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name   string
		sig    syscall.Signal
		needGC bool
	}{
		{"SIGTERM: runtime kills the container and cleans up itself", syscall.SIGTERM, false},
		{"SIGKILL: container dies with the runtime, gc removes the rest", syscall.SIGKILL, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := "gcr-test-" + tc.sig.String()
			cmd := gcrCmd("run", "--", "sh", "-c", "sleep 300 & sleep 301; echo "+marker)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return countProcs("sleep 30") >= 2 })
			cmd.Process.Signal(tc.sig)
			cmd.Wait()
			waitFor(t, func() bool { return countProcs("sleep 30") == 0 })
			if tc.needGC {
				if left, _ := filepath.Glob(filepath.Join(container.StateRoot, "*")); len(left) == 0 {
					t.Fatal("expected a stale state dir for gc to remove")
				}
				if _, errOut, code := gcr(t, "gc"); code != 0 {
					t.Fatalf("gc: %d %s", code, errOut)
				}
			}
			assertNoLeftovers(t)
		})
	}
}

func TestWarmPoolRunsCommandsInFreshContainers(t *testing.T) {
	needRoot(t)
	sock := filepath.Join(t.TempDir(), "gcr.sock")
	srv := gcrCmd("serve", "--pool", "2", "--memory", "64m", "--socket", sock)
	var srvErr bytes.Buffer
	srv.Stderr = &srvErr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		srv.Process.Signal(syscall.SIGTERM)
		srv.Wait()
		if srvErr.Len() > 0 {
			t.Logf("serve stderr: %s", srvErr.String())
		}
		assertNoLeftovers(t)
	}()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })

	out, _, code := gcr(t, "exec", "--socket", sock, "--", "sh", "-c", "echo $$; echo mark > /etc/warm; exit 4")
	if code != 4 || strings.TrimSpace(out) != "1" {
		t.Fatalf("first exec: code %d out %q", code, out)
	}
	for range 3 {
		out, _, code = gcr(t, "exec", "--socket", sock, "--", "sh", "-c", "test -e /etc/warm && echo reused || echo fresh; cat /sys/fs/cgroup/memory.max 2>/dev/null || true")
		if code != 0 || !strings.HasPrefix(out, "fresh") {
			t.Fatalf("container was reused or failed: code %d out %q", code, out)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func countProcs(substr string) int {
	n := 0
	dirs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, d := range dirs {
		b, err := os.ReadFile(d)
		if err == nil && strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), substr) {
			n++
		}
	}
	return n
}
