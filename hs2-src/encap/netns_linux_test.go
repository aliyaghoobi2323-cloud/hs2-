//go:build linux

package encap

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// The raw-socket tests need root (CAP_NET_RAW) and change a per-namespace
// sysctl (icmp_echo_ignore_all), so as root the test binary re-executes itself
// inside a fresh, private network namespace (unshare -n): only a loopback
// interface, nothing of the host touched. Without root they skip.
const netnsEnv = "HS2_ENCAP_NETNS"

func TestMain(m *testing.M) {
	if os.Geteuid() == 0 && os.Getenv(netnsEnv) == "" {
		if path, err := exec.LookPath("unshare"); err == nil {
			cmd := exec.Command(path, append([]string{"-n", "--", os.Args[0]}, os.Args[1:]...)...)
			cmd.Env = append(os.Environ(), netnsEnv+"=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			err := cmd.Run()
			var ee *exec.ExitError
			switch {
			case err == nil:
				os.Exit(0)
			case errors.As(err, &ee):
				os.Exit(ee.ExitCode())
			}
			// unshare itself failed (no permission for namespaces): run here,
			// the raw tests will skip.
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
