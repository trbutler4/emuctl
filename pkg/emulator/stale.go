//go:build unix

package emulator

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReapStale stops emulator processes left running for this device by an
// earlier emuctl, for example after `emuctl serve` was killed. Without this,
// the port guard refuses to start the device until someone kills it by hand.
//
// Only emuctl-created AVDs (emuctl-NAME) whose command line also has this
// device's console port are touched (the launcher and its qemu child both
// carry them). User-supplied AVDs are skipped, and a foreign process on the
// ports still trips the port guard.
func (e *Emulator) ReapStale(ctx context.Context) (int, error) {
	// Never touch a user-supplied AVD: the user may run it by hand.
	if e.Dev.AVDName != "" || e.Dev.AVDHome != "" {
		return 0, nil
	}
	pids, err := e.staleProcesses(ctx)
	if err != nil || len(pids) == 0 {
		return 0, err
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(20 * time.Second)
	for anyAlive(pids) {
		if time.Now().After(deadline) {
			for _, pid := range pids {
				syscall.Kill(pid, syscall.SIGKILL)
			}
			time.Sleep(time.Second)
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if anyAlive(pids) {
		return 0, fmt.Errorf("leftover emulator processes %v did not exit", pids)
	}
	return len(pids), nil
}

func anyAlive(pids []int) bool {
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil {
			return true
		}
	}
	return false
}

func (e *Emulator) staleProcesses(ctx context.Context) ([]int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-ax", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return matchProcesses(string(out), e.avdName(), e.Dev.ConsolePort), nil
}

// matchProcesses parses `ps -o pid=,command=` output and returns PIDs whose
// arguments include `-avd AVD` and `-port PORT`.
func matchProcesses(ps, avd string, port int) []int {
	var pids []int
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid <= 1 {
			continue
		}
		var gotAVD, gotPort bool
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "-avd":
				gotAVD = gotAVD || f[i+1] == avd
			case "-port":
				gotPort = gotPort || f[i+1] == strconv.Itoa(port)
			}
		}
		if gotAVD && gotPort {
			pids = append(pids, pid)
		}
	}
	return pids
}
