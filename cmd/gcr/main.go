// Command gcr runs Linux containers: namespaces, cgroup v2 limits and an
// overlay root filesystem. It needs root.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/dhiyaancnirmal/go-container-runtime/internal/container"
)

const usage = `gcr: a small Linux container runtime

Usage:
  gcr run   [flags] -- COMMAND [ARGS...]   run a command in a new container
  gcr serve [flags]                        keep a pool of warm containers on a socket
  gcr exec  [--socket PATH] -- COMMAND...  run a command in a warm container from serve
  gcr gc                                   remove containers left by a killed runtime

Run "gcr <command> -h" for flags.
`

func main() {
	if container.IsInit() {
		container.InitMain()
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "run":
		code, err = cmdRun(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "exec":
		code, err = cmdExec(os.Args[2:])
	case "gc":
		err = cmdGC()
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gcr: %v\n", err)
		if code == 0 {
			code = 125
		}
	}
	os.Exit(code)
}

const defaultSocket = "/run/gcr.sock"

type specFlags struct {
	rootfs, memory, hostname string
	pids                     int
	cpus                     float64
	hostNet                  bool
}

func (s *specFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&s.rootfs, "rootfs", envOr("GCR_ROOTFS", "rootfs"), "root filesystem directory (lower overlay layer, never written)")
	fs.StringVar(&s.memory, "memory", "", "memory limit, e.g. 64m or 1g (swap disabled)")
	fs.IntVar(&s.pids, "pids", 0, "maximum number of processes")
	fs.Float64Var(&s.cpus, "cpus", 0, "CPU limit in cores, e.g. 0.5")
	fs.BoolVar(&s.hostNet, "host-net", false, "share the host network namespace (default: empty namespace with lo only)")
	fs.StringVar(&s.hostname, "hostname", "", "hostname inside the container (default: container ID)")
}

func (s *specFlags) spec() (container.Spec, error) {
	mem, err := parseBytes(s.memory)
	if err != nil {
		return container.Spec{}, err
	}
	return container.Spec{
		Rootfs:   s.rootfs,
		Hostname: s.hostname,
		HostNet:  s.hostNet,
		Limits:   container.Limits{MemoryBytes: mem, Pids: s.pids, CPUs: s.cpus},
	}, nil
}

func cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var sf specFlags
	sf.register(fs)
	timing := fs.String("timing", "", "write phase timings as JSON to this file")
	fs.Parse(args)
	spec, err := sf.spec()
	if err != nil {
		return 125, err
	}
	spec.Args = fs.Args()
	spec.Stdin, spec.Stdout, spec.Stderr = os.Stdin, os.Stdout, os.Stderr

	c, err := container.Start(spec)
	if err != nil {
		return 125, err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case <-sigs:
			c.Kill()
		case <-c.Done():
		}
	}()
	res, err := c.Wait()
	if *timing != "" {
		b, _ := json.Marshal(res)
		if werr := os.WriteFile(*timing, b, 0o644); werr != nil {
			err = errors.Join(err, werr)
		}
	}
	return res.ExitCode, err
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var sf specFlags
	sf.register(fs)
	socket := fs.String("socket", defaultSocket, "unix socket to listen on")
	size := fs.Int("pool", 4, "number of warm containers to keep ready")
	fs.Parse(args)
	spec, err := sf.spec()
	if err != nil {
		return err
	}
	os.Remove(*socket)
	l, err := net.Listen("unixpacket", *socket)
	if err != nil {
		return err
	}
	pool := container.NewPool(spec, *size)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		l.Close()
	}()
	err = pool.Serve(l)
	pool.Close()
	os.Remove(*socket)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func cmdExec(args []string) (int, error) {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	socket := fs.String("socket", defaultSocket, "socket of a running gcr serve")
	timing := fs.String("timing", "", "write the pool's result and phase timings as JSON to this file")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return 125, errors.New("no command")
	}
	res, err := container.ExecRemote(*socket, fs.Args(), nil)
	if err != nil {
		return 125, err
	}
	if *timing != "" {
		b, _ := json.Marshal(res)
		if err := os.WriteFile(*timing, b, 0o644); err != nil {
			return 125, err
		}
	}
	return res.ExitCode, nil
}

func cmdGC() error {
	removed, err := container.GC()
	for _, id := range removed {
		fmt.Println(id)
	}
	return err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch strings.ToLower(s[len(s)-1:]) {
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}
