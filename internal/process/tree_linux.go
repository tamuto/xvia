package process

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Proc identifies a process. Start (clock ticks since boot) distinguishes a
// process from a later one that reuses the same PID.
type Proc struct {
	Pid   int
	PPid  int
	Start uint64
}

// ProcRoot is the procfs mount point; tests may override it.
var ProcRoot = "/proc"

// ReadStat returns identity information from /proc/<pid>/stat.
func ReadStat(pid int) (Proc, error) {
	b, err := os.ReadFile(filepath.Join(ProcRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return Proc{}, err
	}
	return parseStat(pid, b)
}

// parseStat parses the stat line. The command name (field 2) may contain
// spaces and parentheses, so fields are counted from the last ')'.
func parseStat(pid int, b []byte) (Proc, error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return Proc{}, errors.New("malformed stat")
	}
	f := strings.Fields(string(b[i+1:]))
	// f[0] is field 3 (state); ppid is field 4, starttime field 22.
	if len(f) < 20 {
		return Proc{}, errors.New("malformed stat")
	}
	ppid, err1 := strconv.Atoi(f[1])
	start, err2 := strconv.ParseUint(f[19], 10, 64)
	if err1 != nil || err2 != nil {
		return Proc{}, errors.New("malformed stat")
	}
	return Proc{Pid: pid, PPid: ppid, Start: start}, nil
}

// Descendants returns all live descendants of root (excluding root itself).
//
// It walks /proc/<pid>/task/<tid>/children, which lists the children of
// every thread. On kernels without CONFIG_PROC_CHILDREN it falls back to
// scanning the parent PID of every process. Processes that vanish during the
// walk are skipped.
func Descendants(root int) []Proc {
	if _, err := os.Stat(filepath.Join(ProcRoot, strconv.Itoa(root), "task", strconv.Itoa(root), "children")); err != nil {
		return descendantsByScan(root)
	}
	var out []Proc
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children(pid) {
			if seen[child] {
				continue
			}
			seen[child] = true
			p, err := ReadStat(child)
			if err != nil || p.PPid != pid {
				continue // exited or reparented meanwhile
			}
			out = append(out, p)
			queue = append(queue, child)
		}
	}
	return out
}

func children(pid int) []int {
	tasks, err := os.ReadDir(filepath.Join(ProcRoot, strconv.Itoa(pid), "task"))
	if err != nil {
		return nil
	}
	var out []int
	for _, t := range tasks {
		b, err := os.ReadFile(filepath.Join(ProcRoot, strconv.Itoa(pid), "task", t.Name(), "children"))
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(f); err == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

func descendantsByScan(root int) []Proc {
	entries, err := os.ReadDir(ProcRoot)
	if err != nil {
		return nil
	}
	byParent := map[int][]Proc{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, err := ReadStat(pid); err == nil {
			byParent[p.PPid] = append(byParent[p.PPid], p)
		}
	}
	var out []Proc
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, p := range byParent[pid] {
			out = append(out, p)
			queue = append(queue, p.Pid)
		}
	}
	return out
}
