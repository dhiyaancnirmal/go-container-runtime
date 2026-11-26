package container

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// File descriptors the runtime hands to the init process.
const (
	fdConfig  = 3 // JSON initConfig, closed by the parent after writing
	fdTiming  = 4 // init writes "phase monotonic_ns" lines here
	fdControl = 5 // warm containers only: unix socket to the pool
)

type initConfig struct {
	Rootfs   string   `json:"rootfs"`
	StateDir string   `json:"state_dir"`
	Hostname string   `json:"hostname"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	NetNS    bool     `json:"netns"`
	Warm     bool     `json:"warm"`
}

// execRequest is what a warm container receives when it is handed a command.
type execRequest struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

// InitMain runs inside the new namespaces as PID 1. It builds the root
// filesystem, pivots into it and execs the command. It never returns on success.
func InitMain() {
	if err := initContainer(); err != nil {
		fmt.Fprintf(os.Stderr, "gcr init: %v\n", err)
		os.Exit(127)
	}
}

func initContainer() error {
	timing := os.NewFile(fdTiming, "timing")
	mark := func(phase string) { fmt.Fprintf(timing, "%s %d\n", phase, monotonicNow()) }
	mark("init_start")

	cfgFile := os.NewFile(fdConfig, "config")
	var cfg initConfig
	if err := json.NewDecoder(cfgFile).Decode(&cfg); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfgFile.Close()

	if err := setupRoot(cfg); err != nil {
		return err
	}
	mark("rootfs_ready")

	if cfg.NetNS {
		if err := loopbackUp(); err != nil {
			return fmt.Errorf("bring up lo: %w", err)
		}
	}
	if err := syscall.Sethostname([]byte(cfg.Hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	mark("namespaces_ready")

	args, env := cfg.Args, cfg.Env
	if cfg.Warm {
		req, err := waitForRequest()
		if err != nil {
			return err
		}
		args, env = req.Args, req.Env
		mark("request_received")
	}
	if len(args) == 0 {
		return fmt.Errorf("no command")
	}
	path, err := lookPath(args[0], env)
	if err != nil {
		return err
	}
	mark("exec")
	timing.Close()
	return syscall.Exec(path, args, env)
}

func setupRoot(cfg initConfig) error {
	// Keep every mount below private to this mount namespace.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	merged := filepath.Join(cfg.StateDir, "merged")
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		cfg.Rootfs, filepath.Join(cfg.StateDir, "upper"), filepath.Join(cfg.StateDir, "work"))
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}

	type mnt struct {
		src, target, fstype string
		flags               uintptr
		data                string
	}
	mounts := []mnt{
		{"proc", "proc", "proc", syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV, ""},
		{"sysfs", "sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV | syscall.MS_RDONLY, ""},
		{"tmpfs", "dev", "tmpfs", syscall.MS_NOSUID | syscall.MS_STRICTATIME, "mode=755,size=65536k"},
		{"tmpfs", "tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
	}
	for _, m := range mounts {
		target := filepath.Join(merged, m.target)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.src, target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s: %w", m.target, err)
		}
	}
	if err := populateDev(filepath.Join(merged, "dev")); err != nil {
		return err
	}

	oldroot := filepath.Join(merged, ".oldroot")
	if err := os.MkdirAll(oldroot, 0o700); err != nil {
		return err
	}
	if err := syscall.PivotRoot(merged, oldroot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return err
	}
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove("/.oldroot")
}

func populateDev(dev string) error {
	nodes := []struct {
		name         string
		major, minor uint32
	}{
		{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7},
		{"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0},
	}
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	for _, n := range nodes {
		devnum := int((n.major << 8) | n.minor)
		if err := syscall.Mknod(filepath.Join(dev, n.name), syscall.S_IFCHR|0o666, devnum); err != nil {
			return fmt.Errorf("mknod %s: %w", n.name, err)
		}
	}
	links := map[string]string{
		"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dev, name)); err != nil {
			return err
		}
	}
	return nil
}

// loopbackUp sets IFF_UP on lo so programs inside the sandbox can use 127.0.0.1.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var ifr struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(ifr.name[:], "lo")
	ifr.flags = syscall.IFF_UP | syscall.IFF_RUNNING
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return e
	}
	return nil
}

// waitForRequest tells the pool this container is ready, then blocks until
// the pool sends a command plus the caller's stdin, stdout and stderr.
func waitForRequest() (execRequest, error) {
	var req execRequest
	f := os.NewFile(fdControl, "control")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return req, fmt.Errorf("control socket: %w", err)
	}
	uc := conn.(*net.UnixConn)
	defer uc.Close()
	if _, err := uc.Write([]byte{1}); err != nil {
		return req, fmt.Errorf("signal ready: %w", err)
	}
	buf := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(3*4))
	n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return req, fmt.Errorf("read request: %w", err)
	}
	if n == 0 {
		return req, io.EOF
	}
	fds, err := parseRights(oob[:oobn])
	if err != nil {
		return req, err
	}
	if len(fds) != 3 {
		return req, fmt.Errorf("want 3 stdio fds, got %d", len(fds))
	}
	for i, fd := range fds {
		if err := syscall.Dup2(fd, i); err != nil {
			return req, err
		}
		syscall.Close(fd)
	}
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return req, fmt.Errorf("decode request: %w", err)
	}
	return req, nil
}

func parseRights(oob []byte) ([]int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, m := range msgs {
		r, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return nil, err
		}
		fds = append(fds, r...)
	}
	return fds, nil
}

// lookPath resolves name against PATH from the container's own environment.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: command not found", name)
}
