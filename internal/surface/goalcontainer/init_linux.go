package goalcontainer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func RunInit(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(arguments) == 1 && arguments[0] == "--protocol" {
		identity, err := processProbe()
		if err != nil || termsafe.JSONEncoder(stdout).Encode(identity) != nil {
			return 125
		}
		return 0
	}
	if os.Getpid() != 1 || os.Geteuid() != 0 || len(arguments) < 5 {
		_, _ = fmt.Fprintln(stderr, "container init requires its private Linux PID namespace and fixed command")
		return 125
	}
	identity, err := processProbe()
	if err != nil || identity.Effective != "00000000000000c0" || identity.Permitted != "00000000000000c0" || identity.Inherited != "0000000000000000" || identity.Ambient != "0000000000000000" || identity.NoNewPrivileges != "1" || identity.Seccomp != "2" {
		_, _ = fmt.Fprintln(stderr, "container init policy is unavailable")
		return 125
	}
	uid, uidErr := strconv.ParseUint(arguments[0], 10, 32)
	gid, gidErr := strconv.ParseUint(arguments[1], 10, 32)
	deadline, timeErr := strconv.ParseInt(arguments[2], 10, 64)
	duration := time.Until(time.Unix(0, deadline))
	if uidErr != nil || gidErr != nil || timeErr != nil || uid == 0 || gid == 0 || duration <= 0 || duration > 120*time.Second || !cleanPath(arguments[3]) || stdin == nil {
		_, _ = fmt.Fprintln(stderr, "container init received an invalid identity, deadline or working directory")
		return 125
	}
	commandContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandContext, arguments[4], arguments[5:]...)
	command.Dir = arguments[3]
	command.Env = workloadEnvironment(os.Getenv("GOCACHE"))
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, Setpgid: true}
	finished := make(chan error, 1)
	go func() { finished <- command.Run() }()
	beats, closed := make(chan struct{}, 1), make(chan struct{})
	go func() {
		defer close(closed)
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 64), 64)
		for scanner.Scan() {
			if scanner.Text() != "alive" {
				return
			}
			select {
			case beats <- struct{}{}:
			default:
			}
		}
	}()
	lease, hardDeadline := time.NewTimer(leaseTimeout), time.NewTimer(duration)
	defer lease.Stop()
	defer hardDeadline.Stop()
	for {
		select {
		case <-beats:
			lease.Reset(leaseTimeout)
		case <-closed:
			return 124
		case <-lease.C:
			return 124
		case <-hardDeadline.C:
			return 124
		case <-ctx.Done():
			return 124
		case err := <-finished:
			if err == nil {
				return 0
			}
			var exited *exec.ExitError
			if errors.As(err, &exited) {
				if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal())
				}
				return exited.ExitCode()
			}
			_, _ = fmt.Fprintln(stderr, "container command could not start:", err)
			return 125
		}
	}
}

func processProbe() (probe, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return probe{}, err
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found {
			fields[key] = strings.TrimSpace(value)
		}
	}
	groups, err := os.Getgroups()
	if err != nil {
		return probe{}, err
	}
	return probe{Protocol: Protocol, UID: os.Geteuid(), GID: os.Getegid(), Groups: groups, Effective: fields["CapEff"], Permitted: fields["CapPrm"], Inherited: fields["CapInh"], Ambient: fields["CapAmb"], NoNewPrivileges: fields["NoNewPrivs"], Seccomp: fields["Seccomp"]}, nil
}
