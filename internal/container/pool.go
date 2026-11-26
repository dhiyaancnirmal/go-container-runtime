package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
)

// Pool keeps containers started ahead of time: cgroup created, overlay
// mounted, namespaces entered, root pivoted, init parked waiting for a
// command. Handing one a command skips everything except exec.
type Pool struct {
	tmpl  Spec
	size  int
	ready chan *warm

	mu     sync.Mutex
	closed bool
	live   map[*Container]struct{} // every container started, idle or running
	wg     sync.WaitGroup          // refills, in-flight execs and their cleanups
}

// track records c so Close can kill it. If the pool is already closing, c is
// killed at once.
func (p *Pool) track(c *Container) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for old := range p.live {
		select {
		case <-old.Done():
			delete(p.live, old)
		default:
		}
	}
	p.live[c] = struct{}{}
	if p.closed {
		c.Kill()
	}
}

// begin registers one unit of work with the pool unless it is closing.
func (p *Pool) begin() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.wg.Add(1)
	return true
}

type warm struct {
	c      *Container
	fillNS int64 // Start until init reported ready
}

// ExecResult describes one command run through the pool.
type ExecResult struct {
	ExitCode int     `json:"exit_code"`
	PoolHit  bool    `json:"pool_hit"` // false when the pool was empty and a container was started on demand
	FillNS   int64   `json:"fill_ns"`  // cost of preparing the container that ran this command
	Phases   []Phase `json:"phases"`
	Error    string  `json:"error,omitempty"`
}

// NewPool starts size warm containers from tmpl. Args in tmpl are ignored.
func NewPool(tmpl Spec, size int) *Pool {
	tmpl.ID, tmpl.Hostname, tmpl.Args = "", "", nil
	tmpl.Stdin, tmpl.Stdout, tmpl.Stderr = nil, nil, nil
	p := &Pool{tmpl: tmpl, size: size, ready: make(chan *warm, size), live: map[*Container]struct{}{}}
	for range size {
		p.refill()
	}
	return p
}

func (p *Pool) startWarm() (*warm, error) {
	spec := p.tmpl
	spec.warm = true
	t0 := monotonicNow()
	c, err := Start(spec)
	if err != nil {
		return nil, err
	}
	p.track(c)
	var b [1]byte
	if _, err := io.ReadFull(c.control, b[:]); err != nil {
		c.Kill()
		res, _ := c.Wait()
		return nil, fmt.Errorf("warm container %s exited before ready (exit %d)", c.ID, res.ExitCode)
	}
	return &warm{c: c, fillNS: monotonicNow() - t0}, nil
}

func (p *Pool) refill() {
	if !p.begin() {
		return
	}
	go func() {
		defer p.wg.Done()
		w, err := p.startWarm()
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if err != nil {
			if !closed { // during Close, killed refills are expected
				log.Printf("pool refill: %v", err)
			}
			return
		}
		if closed {
			w.c.Kill()
			w.c.Wait()
			return
		}
		p.ready <- w
	}()
}

// Exec runs args in a warm container with the given stdio and waits for it.
func (p *Pool) Exec(args, env []string, stdio [3]*os.File) (ExecResult, error) {
	if len(args) == 0 {
		return ExecResult{}, errors.New("no command")
	}
	if env == nil {
		env = DefaultEnv
	}
	if !p.begin() {
		return ExecResult{}, errors.New("pool is shutting down")
	}
	defer p.wg.Done()
	var w *warm
	hit := true
	select {
	case w = <-p.ready:
	default:
		hit = false
		var err error
		if w, err = p.startWarm(); err != nil {
			return ExecResult{}, err
		}
	}
	if p.size > 0 {
		p.refill()
	}
	c := w.c
	c.mark("request_sent")
	msg, _ := json.Marshal(execRequest{Args: args, Env: env})
	rights := syscall.UnixRights(int(stdio[0].Fd()), int(stdio[1].Fd()), int(stdio[2].Fd()))
	if err := syscall.Sendmsg(int(c.control.Fd()), msg, rights, nil, 0); err != nil {
		c.Kill()
		c.Wait()
		return ExecResult{}, fmt.Errorf("send request: %w", err)
	}
	// Reply as soon as the command exits; tear the container down off the
	// request path, like the refill.
	code := c.WaitExit()
	c.mark("exited")
	phases := mergePhases(c.phases, readPhases(c.timing))
	c.timing.Close()
	c.timing = nil
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if _, err := c.Wait(); err != nil {
			log.Printf("cleanup %s: %v", c.ID, err)
		}
	}()
	return ExecResult{ExitCode: code, PoolHit: hit, FillNS: w.fillNS, Phases: phases}, nil
}

// Close kills every container, idle or running, and waits until each one's
// cgroup and state directory are removed.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	for c := range p.live {
		c.Kill()
	}
	p.mu.Unlock()
	p.wg.Wait()
	for {
		select {
		case w := <-p.ready:
			w.c.Kill()
			w.c.Wait()
		default:
			return
		}
	}
}

// Serve accepts exec requests on a unix seqpacket socket. Each request is a
// JSON execRequest with the client's stdin, stdout and stderr attached.
func (p *Pool) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go p.handle(conn.(*net.UnixConn))
	}
}

func (p *Pool) handle(conn *net.UnixConn) {
	defer conn.Close()
	buf := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(3*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}
	fds, err := parseRights(oob[:oobn])
	var files [3]*os.File
	if err == nil && len(fds) == 3 {
		for i, fd := range fds {
			files[i] = os.NewFile(uintptr(fd), "stdio")
		}
		defer func() {
			for _, f := range files {
				f.Close()
			}
		}()
	} else {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		err = fmt.Errorf("want 3 stdio fds, got %d", len(fds))
	}
	var res ExecResult
	if err == nil {
		var req execRequest
		if err = json.Unmarshal(buf[:n], &req); err == nil {
			res, err = p.Exec(req.Args, req.Env, files)
		}
	}
	if err != nil {
		res.ExitCode = 125
		res.Error = err.Error()
	}
	out, _ := json.Marshal(res)
	conn.Write(out)
}

// ExecRemote sends args to a pool listening on socket, attaching this
// process's stdio, and returns the result.
func ExecRemote(socket string, args, env []string) (ExecResult, error) {
	var res ExecResult
	conn, err := net.Dial("unixpacket", socket)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	uc := conn.(*net.UnixConn)
	msg, _ := json.Marshal(execRequest{Args: args, Env: env})
	rights := syscall.UnixRights(0, 1, 2)
	if _, _, err := uc.WriteMsgUnix(msg, rights, nil); err != nil {
		return res, err
	}
	buf := make([]byte, 64*1024)
	n, err := uc.Read(buf)
	if err != nil {
		return res, fmt.Errorf("read result: %w", err)
	}
	if err := json.Unmarshal(buf[:n], &res); err != nil {
		return res, err
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}
