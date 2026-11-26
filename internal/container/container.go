// Package container is a small Linux container runtime: namespaces,
// cgroup v2 limits, an overlay root filesystem and full cleanup.
package container

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// StateRoot holds one directory per container (overlay upper, work, merged).
var StateRoot = "/run/gcr"

// DefaultEnv is the environment a command gets when the caller passes none.
var DefaultEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"TERM=xterm",
}

// Spec describes one container.
type Spec struct {
	ID       string // generated when empty
	Rootfs   string // read-only lower layer; writes go to a per-container overlay
	Args     []string
	Env      []string
	Hostname string // defaults to the ID
	Limits   Limits
	HostNet  bool // share the host network namespace instead of an empty one
	Stdin    *os.File
	Stdout   *os.File
	Stderr   *os.File

	warm bool
}

// Phase is a named point in time on the CLOCK_MONOTONIC clock.
type Phase struct {
	Name string `json:"name"`
	NS   int64  `json:"ns"`
}

// Result is what Wait returns.
type Result struct {
	ID       string  `json:"id"`
	ExitCode int     `json:"exit_code"`
	Phases   []Phase `json:"phases"` // ordered, absolute monotonic ns
}

// Container is a started container.
type Container struct {
	ID       string
	cgroup   string
	stateDir string
	cmd      *exec.Cmd
	timing   *os.File
	control  *os.File // warm only: pool end of the control socket
	phases   []Phase
	exited   chan struct{}
	cmdErr   error

	waitOnce sync.Once
	result   Result
	waitErr  error
	done     chan struct{}
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func monotonicNow() int64 {
	var ts syscall.Timespec
	const clockMonotonic = 1
	syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic, uintptr(unsafe.Pointer(&ts)), 0)
	return ts.Nano()
}

// Run starts a container and waits for it to exit and be cleaned up.
func Run(spec Spec) (Result, error) {
	c, err := Start(spec)
	if err != nil {
		return Result{}, err
	}
	return c.Wait()
}

// Start creates the cgroup and state directory and clones the init process
// into new namespaces inside that cgroup.
func Start(spec Spec) (c *Container, err error) {
	start := monotonicNow()
	if spec.Rootfs == "" {
		return nil, errors.New("rootfs is required")
	}
	rootfs, err := filepath.Abs(spec.Rootfs)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(rootfs, "bin")); err != nil {
		return nil, fmt.Errorf("rootfs %s does not look like a root filesystem: %w", rootfs, err)
	}
	if spec.ID == "" {
		spec.ID = newID()
	}
	if spec.Hostname == "" {
		spec.Hostname = spec.ID
	}
	if spec.Env == nil {
		spec.Env = DefaultEnv
	}
	if !spec.warm && len(spec.Args) == 0 {
		return nil, errors.New("no command")
	}

	c = &Container{ID: spec.ID, done: make(chan struct{})}
	c.phases = append(c.phases, Phase{"start", start})
	defer func() {
		if err != nil {
			c.cleanup()
		}
	}()

	c.stateDir = filepath.Join(StateRoot, spec.ID)
	for _, d := range []string{"upper", "work", "merged"} {
		if err := os.MkdirAll(filepath.Join(c.stateDir, d), 0o700); err != nil {
			return c, fmt.Errorf("create state dir: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(c.stateDir, "owner"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return c, err
	}
	c.mark("state_dir")

	path, cgfd, err := createCgroup(spec.ID, spec.Limits)
	if err != nil {
		return c, err
	}
	c.cgroup = path
	defer cgfd.Close()
	c.mark("cgroup")

	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return c, err
	}
	defer cfgR.Close()
	defer cfgW.Close()
	timR, timW, err := os.Pipe()
	if err != nil {
		return c, err
	}
	c.timing = timR
	defer timW.Close()
	extra := []*os.File{cfgR, timW}

	var childCtl *os.File
	if spec.warm {
		pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return c, err
		}
		c.control = os.NewFile(uintptr(pair[0]), "control")
		childCtl = os.NewFile(uintptr(pair[1]), "control-child")
		defer childCtl.Close()
		extra = append(extra, childCtl)
	}

	self, err := os.Executable()
	if err != nil {
		return c, err
	}
	cmd := exec.Command(self)
	cmd.Args = []string{"gcr-init"}
	cmd.Env = []string{InitEnv + "=1"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	cmd.ExtraFiles = extra
	flags := syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWPID
	if !spec.HostNet {
		flags |= syscall.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  uintptr(flags),
		UseCgroupFD: true,
		CgroupFD:    int(cgfd.Fd()),
		Pdeathsig:   syscall.SIGKILL,
	}

	// Pdeathsig fires when the thread that forked exits, not the process.
	// Fork and reap from one goroutine locked to its OS thread so the Go
	// scheduler cannot retire that thread while the container runs.
	started := make(chan error, 1)
	c.exited = make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		c.cmdErr = cmd.Wait()
		close(c.exited)
	}()
	if err := <-started; err != nil {
		return c, fmt.Errorf("start init: %w", err)
	}
	c.cmd = cmd
	c.mark("clone")

	cfg := initConfig{
		Rootfs: rootfs, StateDir: c.stateDir, Hostname: spec.Hostname,
		Args: spec.Args, Env: spec.Env, NetNS: !spec.HostNet, Warm: spec.warm,
	}
	if err := json.NewEncoder(cfgW).Encode(cfg); err != nil {
		return c, fmt.Errorf("send config: %w", err)
	}
	return c, nil
}

func (c *Container) mark(name string) {
	c.phases = append(c.phases, Phase{name, monotonicNow()})
}

// Kill sends SIGKILL to every process in the container.
func (c *Container) Kill() error {
	if c.cgroup == "" {
		return nil
	}
	return killCgroup(c.cgroup)
}

// Done is closed when Wait has finished.
func (c *Container) Done() <-chan struct{} { return c.done }

// Wait waits for the container's init process to exit, then removes the
// cgroup and state directory. It is safe to call more than once.
func (c *Container) Wait() (Result, error) {
	c.waitOnce.Do(func() {
		defer close(c.done)
		c.result.ID = c.ID
		<-c.exited
		c.mark("exited")
		c.result.ExitCode = exitCode(c.cmd.ProcessState, c.cmdErr)
		var childPhases []Phase
		if c.timing != nil {
			childPhases = readPhases(c.timing)
		}
		c.waitErr = c.cleanup()
		c.mark("cleaned_up")
		c.result.Phases = mergePhases(c.phases, childPhases)
	})
	return c.result, c.waitErr
}

func (c *Container) cleanup() error {
	var errs []error
	if c.control != nil {
		c.control.Close()
	}
	if c.timing != nil {
		c.timing.Close()
	}
	if c.cgroup != "" {
		errs = append(errs, removeCgroup(c.cgroup))
	}
	if c.stateDir != "" {
		errs = append(errs, os.RemoveAll(c.stateDir))
	}
	return errors.Join(errs...)
}

func exitCode(ps *os.ProcessState, err error) int {
	if ps == nil {
		return 127
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if ps.ExitCode() >= 0 {
		return ps.ExitCode()
	}
	if err != nil {
		return 127
	}
	return 0
}

func readPhases(f *os.File) []Phase {
	var out []Phase
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, ns, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(ns, 10, 64)
		if err == nil {
			out = append(out, Phase{name, v})
		}
	}
	return out
}

// mergePhases interleaves parent and child phases by timestamp. The clock is
// shared because the runtime does not create a time namespace.
func mergePhases(a, b []Phase) []Phase {
	out := make([]Phase, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if j >= len(b) || (i < len(a) && a[i].NS <= b[j].NS) {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return out
}

// InitEnv marks a re-executed process as a container init.
const InitEnv = "_GCR_INIT"

// IsInit reports whether this process was started as a container init.
func IsInit() bool { return os.Getenv(InitEnv) == "1" }
