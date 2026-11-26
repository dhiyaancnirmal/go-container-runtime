package container

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// GC removes containers whose owning runtime process is gone, for example
// after the runtime was SIGKILLed. It returns the IDs it removed.
func GC() ([]string, error) {
	entries, err := os.ReadDir(StateRoot)
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var removed []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		seen[id] = true
		if ownerAlive(filepath.Join(StateRoot, id, "owner")) {
			continue
		}
		errs = append(errs, removeCgroup(filepath.Join(CgroupRoot, id)), os.RemoveAll(filepath.Join(StateRoot, id)))
		removed = append(removed, id)
	}
	// Cgroups with no state dir left behind are orphans too.
	cgs, err := os.ReadDir(CgroupRoot)
	if err == nil {
		for _, e := range cgs {
			if e.IsDir() && !seen[e.Name()] {
				errs = append(errs, removeCgroup(filepath.Join(CgroupRoot, e.Name())))
				removed = append(removed, e.Name())
			}
		}
	}
	return removed, errors.Join(errs...)
}

func ownerAlive(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
