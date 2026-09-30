//go:build linux

package encap

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// The raw-socket tests need root (CAP_NET_RAW) and change a per-namespace
// sysctl (icmp_echo_ignore_all), so as root the test binary re-executes itself
// inside a fresh, private network namespace (unshare -n): only a loopback
// interface, nothing of the host touched. Without root they skip.
const netnsEnv = "HS2_ENCAP_NETNS"

// netnsPermitted reports whether this process can create a private network
// namespace. It probes in a throwaway goroutine that locks its OS thread and
// never unlocks: when the goroutine returns the runtime destroys that thread,
// discarding the new netns it entered, so the probe leaves the process's own
// threads (and their netns) untouched.
func netnsPermitted() bool {
	done := make(chan bool, 1)
	go func() {
		runtime.LockOSThread() // intentionally not unlocked: thread is discarded
		done <- unix.Unshare(unix.CLONE_NEWNET) == nil
	}()
	return <-done
}

func TestMain(m *testing.M) {
	if os.Geteuid() == 0 && os.Getenv(netnsEnv) == "" {
		// Re-exec into a private netns ONLY when we can actually create one. As
		// root inside a restricted container (e.g. Docker without CAP_SYS_ADMIN,
		// or a seccomp/userns policy that blocks CLONE_NEWNET) `unshare -n` exits
		// 1 because it cannot set up the namespace — not because a test failed.
		// Propagating that exit code would fail the whole package with zero tests
		// run. So probe first: if a netns is not permitted, fall through and run
		// in-process, where the raw tests SKIP (needRawNetns) instead of failing.
		// The happy path (root with CAP_SYS_ADMIN) still re-execs as before.
		if path, err := exec.LookPath("unshare"); err == nil && netnsPermitted() {
			cmd := exec.Command(path, append([]string{"-n", "--", os.Args[0]}, os.Args[1:]...)...)
			cmd.Env = append(os.Environ(), netnsEnv+"=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			err := cmd.Run()
			var ee *exec.ExitError
			switch {
			case err == nil:
				os.Exit(0)
			case errors.As(err, &ee):
				// The child ran the tests (it set up the namespace, since the
				// probe confirmed it could) and this is its real test exit code.
				os.Exit(ee.ExitCode())
			}
			// Some other failure launching the child (not a test result): fall
			// through and run in-process, where the raw tests skip.
		}
	}
	if os.Getenv(netnsEnv) == "1" {
		if err := linkUp("lo"); err != nil {
			os.Stderr.WriteString("encap tests: bring lo up: " + err.Error() + "\n")
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// inPrivateNetns reports whether this process runs in the private namespace
// the raw tests need.
func inPrivateNetns() bool { return os.Getenv(netnsEnv) == "1" }

func needRawNetns(t *testing.T) {
	t.Helper()
	if !inPrivateNetns() {
		t.Skip("raw-socket tests need root and a private network namespace (unshare -n)")
	}
}

// linkUp sets IFF_UP on an interface (ip link set <if> up) without iproute2.
func linkUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
