package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CgroupRoot is the cgroup v2 directory that holds one leaf per container.
var CgroupRoot = "/sys/fs/cgroup/gcr"

// Limits are the cgroup v2 limits applied to a container. Zero means unlimited.
type Limits struct {
	MemoryBytes int64   // memory.max; swap is disabled when set
	Pids        int     // pids.max
	CPUs        float64 // cpu.max quota as a fraction of one CPU per 100ms period
}

func ensureCgroupParent() error {
	if err := os.MkdirAll(CgroupRoot, 0o755); err != nil {
		return fmt.Errorf("create cgroup parent: %w", err)
	}
	ctl := filepath.Join(CgroupRoot, "cgroup.subtree_control")
	cur, err := os.ReadFile(ctl)
	if err != nil {
		return fmt.Errorf("read %s: %w", ctl, err)
	}
	var want []string
	for _, c := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(" "+strings.TrimSpace(string(cur))+" ", " "+c+" ") {
			want = append(want, "+"+c)
		}
	}
	if len(want) == 0 {
		return nil
	}
	if err := os.WriteFile(ctl, []byte(strings.Join(want, " ")), 0); err != nil {
		return fmt.Errorf("enable controllers %v in %s: %w", want, CgroupRoot, err)
	}
	return nil
}

// createCgroup makes the container's leaf cgroup, writes its limits and
// returns an open directory fd for clone3's CLONE_INTO_CGROUP.
func createCgroup(id string, l Limits) (string, *os.File, error) {
	if err := ensureCgroupParent(); err != nil {
		return "", nil, err
	}
	path := filepath.Join(CgroupRoot, id)
	if err := os.Mkdir(path, 0o755); err != nil {
		return "", nil, fmt.Errorf("create cgroup: %w", err)
	}
	write := func(file, val string) error {
		if err := os.WriteFile(filepath.Join(path, file), []byte(val), 0); err != nil {
			return fmt.Errorf("set %s=%s: %w", file, val, err)
		}
		return nil
	}
	var err error
	if l.MemoryBytes > 0 {
		err = errors.Join(err, write("memory.max", strconv.FormatInt(l.MemoryBytes, 10)))
		// On OOM, kill the whole container rather than one process in it.
		err = errors.Join(err, write("memory.oom.group", "1"))
		if _, statErr := os.Stat(filepath.Join(path, "memory.swap.max")); statErr == nil {
			err = errors.Join(err, write("memory.swap.max", "0"))
		}
	}
	if l.Pids > 0 {
		err = errors.Join(err, write("pids.max", strconv.Itoa(l.Pids)))
	}
	if l.CPUs > 0 {
		const period = 100000
		err = errors.Join(err, write("cpu.max", fmt.Sprintf("%d %d", int(l.CPUs*period), period)))
	}
	if err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	fd, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	return path, fd, nil
}

// killCgroup kills every process in the cgroup.
func killCgroup(path string) error {
	err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// removeCgroup kills what is left in the cgroup and removes it, retrying
// while the kernel finishes reaping.
func removeCgroup(path string) error {
	if err := killCgroup(path); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Rmdir(path)
		if err == nil || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return fmt.Errorf("remove cgroup %s: %w", path, err)
		}
		time.Sleep(200 * time.Microsecond)
	}
}
