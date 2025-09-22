// linkerdev.go — Telepresence-lite via in-cluster relay (Docker-driver friendly)
// Build: go build -o linkerdev .
// Usage: linkerdev -svc <name.ns> -p <local-port> <your app cmd> [args...]
// Commands:
//   linkerdev install         (install relay component in cluster)
//   linkerdev uninstall       (remove relay component from cluster)
//   linkerdev clean           (best-effort sweep of leftover resources)
//   linkerdev version         (show version information)
//   linkerdev help            (show help message)

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sopatech/linkerdev/cmd/linkerdev/internal/child"
	"github.com/sopatech/linkerdev/cmd/linkerdev/internal/commands"
	"github.com/sopatech/linkerdev/cmd/linkerdev/internal/k8s"
	"github.com/sopatech/linkerdev/cmd/linkerdev/internal/linkerd"
	"github.com/sopatech/linkerdev/cmd/linkerdev/internal/relay"
	"github.com/sopatech/linkerdev/cmd/linkerdev/internal/tun"

	coordv1 "k8s.io/api/coordination/v1"
	"k8s.io/client-go/dynamic"
)

const version = "v0.1.0-alpha17"

func main() {
	var (
		svcFlag  = flag.String("svc", "", "Service name in format 'name.namespace'")
		portFlag = flag.Int("p", 0, "Local port to forward to")
	)
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		commands.ShowHelp()
		os.Exit(1)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "help":
		commands.ShowHelp()
		return
	case "version":
		fmt.Printf("linkerdev version %s\n", version)
		return
	case "install":
		commands.InstallRelay(version)
		return
	case "uninstall":
		commands.UninstallRelay()
		return
	case "clean":
		// Cleanup mode - clean up resources
		cs := k8s.NewClientset()
		cfg := k8s.LoadKubeConfigOrDie()
		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			log.Fatalf("Failed to create dynamic client: %v", err)
		}

		// Clean up all resources
		ctx := context.Background()
		commands.CleanupResources(ctx, cs, dyn, "default", "")
		return
	}

	// Main proxy mode
	if *svcFlag == "" || *portFlag == 0 {
		log.Fatal("Both -svc and -p flags are required for proxy mode")
	}

	parts := strings.Split(*svcFlag, ".")
	if len(parts) != 2 {
		log.Fatal("Service must be in format 'name.namespace'")
	}
	svcName, ns := parts[0], parts[1]

	if !isValidKubernetesName(svcName) || !isValidKubernetesName(ns) {
		log.Fatal("Invalid service or namespace name")
	}

	// Generate instance ID
	instance := instanceID()
	log.Printf("Starting linkerdev for service %s in namespace %s (instance: %s)", svcName, ns, instance)

	// Load Kubernetes config and create clients
	cs := k8s.NewClientset()
	cfg := k8s.LoadKubeConfigOrDie()
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Received signal, shutting down...")
		cancel()
	}()

	// Create lease for instance tracking
	leaseName := svcName + "-" + instance
	lease := mustLease(k8s.EnsureLease(ctx, cs, ns, leaseName, instance)).(*coordv1.Lease)

	// Start lease renewal
	leaseStop := make(chan struct{})
	defer close(leaseStop)
	go k8s.RenewLeaseLoop(cs, ns, leaseName, instance, leaseStop)

	// Ensure relay is running
	remoteRPort := int32(remotePortFor(svcName, ns))
	_, err = k8s.EnsureRelay(ctx, cs, ns, remoteRPort, lease, instance, version)
	if err != nil {
		log.Fatalf("Failed to ensure relay: %v", err)
	}

	// Start port forward to relay
	localRPort, err := randomFreePort()
	if err != nil {
		log.Fatalf("Failed to get free port: %v", err)
	}

	pfCmd, err := k8s.StartKubectlPortForward(ctx, ns, "linkerdev-relay", localRPort, int(remoteRPort))
	if err != nil {
		log.Fatalf("Failed to start port forward: %v", err)
	}
	defer pfCmd.Process.Kill()

	// Wait for port forward to be ready
	time.Sleep(2 * time.Second)

	// Connect to relay
	rc, err := relay.NewRelayClient(fmt.Sprintf("127.0.0.1:%d", localRPort))
	if err != nil {
		log.Fatalf("Failed to connect to relay: %v", err)
	}
	defer rc.Close()

	// Create dev service and endpoint slice
	devSvcName := svcName + "-dev"
	err = k8s.EnsureDevService(ctx, cs, ns, svcName, 80, remoteRPort, lease, instance)
	if err != nil {
		log.Fatalf("Failed to create dev service: %v", err)
	}

	// Get relay pod IP for endpoint slice
	relayIP, err := k8s.WaitForPodIP(ctx, cs, ns, "app=linkerdev-relay", 30*time.Second)
	if err != nil {
		log.Fatalf("Failed to get relay pod IP: %v", err)
	}

	err = k8s.EnsureDevEndpointSlice(ctx, cs, ns, svcName, relayIP, remoteRPort, lease, instance)
	if err != nil {
		log.Fatalf("Failed to create endpoint slice: %v", err)
	}

	// Create TrafficSplit to route 100% traffic to dev service
	err = linkerd.ApplyTrafficSplit(ctx, dyn, ns, svcName, devSvcName, lease, instance)
	if err != nil {
		log.Fatalf("Failed to create TrafficSplit: %v", err)
	}

	// Start TUN interface for transparent proxying
	go tun.StartTransparentProxy(ctx, cs, rc)

	// Start relay client loop
	go rc.Loop(*portFlag)

	// Start child process
	childCmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	childCmd.Stdin = os.Stdin
	childCmd.Stdout = os.Stdout
	childCmd.Stderr = os.Stderr

	// Run child with custom resolv.conf
	err = child.RunChildWithResolv(ctx, childCmd)
	if err != nil {
		log.Printf("Child process exited with error: %v", err)
	}

	// Cleanup
	log.Println("Cleaning up resources...")
	linkerd.DeleteTrafficSplit(ctx, dyn, ns, svcName+"-split")
	commands.CleanupResources(ctx, cs, dyn, ns, svcName)
}

// Utility functions
func isValidKubernetesName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	// Kubernetes names must be lowercase alphanumeric characters and hyphens
	// Cannot start or end with hyphen
	if name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

func instanceID() string {
	h, _ := os.Hostname()
	// Sanitize hostname to be a valid Kubernetes name
	sanitized := strings.ToLower(h)
	sanitized = strings.ReplaceAll(sanitized, ".", "-")
	// Remove any invalid characters (keep only alphanumeric and hyphens)
	var result strings.Builder
	for _, c := range sanitized {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result.WriteRune(c)
		}
	}
	sanitized = result.String()
	// Ensure it doesn't start or end with hyphen
	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		sanitized = "host"
	}
	return fmt.Sprintf("%s-%d", sanitized, os.Getpid())
}

func mustLease(l interface{}, err error) interface{} {
	if err != nil {
		log.Fatal(err)
	}
	return l
}

func remotePortFor(svc, ns string) int {
	// Simple hash-based port generation
	h := 0
	for _, c := range ns + "/" + svc {
		h = h*31 + int(c)
	}
	return 20000 + (h % 10000) // 20000..29999
}

func randomFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	_, pStr, _ := net.SplitHostPort(l.Addr().String())
	return strconv.Atoi(pStr)
}
