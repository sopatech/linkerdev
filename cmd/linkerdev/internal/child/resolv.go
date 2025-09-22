package child

import (
	"context"
	"os/exec"
	"runtime"
)

// RunChildWithResolv runs a child process with custom resolv.conf handling
func RunChildWithResolv(ctx context.Context, child *exec.Cmd) error {
	// Create a temporary resolv.conf that only resolves cluster services
	resolv := `nameserver 127.0.0.1
search default.svc.cluster.local svc.cluster.local cluster.local
options ndots:5
`

	switch runtime.GOOS {
	case "linux":
		return runChildWithResolvLinux(ctx, child, resolv)
	case "darwin":
		return runChildWithResolvDarwin(ctx, child, resolv)
	default:
		// Fallback to running without custom resolv.conf
		return child.Run()
	}
}

// runChildWithResolvLinux runs child with custom resolv.conf on Linux
func runChildWithResolvLinux(ctx context.Context, child *exec.Cmd, resolv string) error {
	// Use unshare to create a new network namespace with custom resolv.conf
	child.Path = "unshare"
	child.Args = append([]string{"unshare", "--net", "--pid", "--fork", "--mount-proc", "sh", "-c",
		"echo '" + resolv + "' > /etc/resolv.conf && exec " + child.Args[0]}, child.Args[1:]...)

	return child.Run()
}

// runChildWithResolvDarwin runs child with custom resolv.conf on macOS
func runChildWithResolvDarwin(ctx context.Context, child *exec.Cmd, resolv string) error {
	// On macOS, we can't easily modify resolv.conf per-process
	// So we'll set environment variables that some resolvers respect
	child.Env = append(child.Env,
		"RESOLV_CONF="+resolv,
		"DNS_SERVERS=127.0.0.1",
		"DNS_SEARCH=default.svc.cluster.local svc.cluster.local cluster.local",
	)

	return child.Run()
}
