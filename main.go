// linkerdev.go — Telepresence-lite for macOS & Linux using Linkerd dynamic request routing
// Build: go build -o linkerdev .
// Usage: ./linkerdev -svc <name.ns> -p <local-port> <your app command> [args...]

package main

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
	appsV1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

/* ---------- Config / constants ---------- */

const (
	sshUser = "docker" // user on your minikube node (often "docker" or "docker" on Docker Desktop VM)
	sshBin  = "ssh"
	sshArgs = "-o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o TCPKeepAlive=yes -o StrictHostKeyChecking=no"

	// Lease/cleanup
	leaseRenewEvery = 10 * time.Second
	leaseStaleAfter = 2 * time.Minute
	kdvLabelOwned   = "app.kdvwrap/owned"
	kdvLabelInst    = "app.kdvwrap/instance"

	// macOS resolver
	resolverDir  = "/etc/resolver"
	resolverFile = "/etc/resolver/svc.cluster.local"

	// Cluster SSH daemon
	sshdNS       = "kube-system" // or pick a dev ns you prefer
	sshdName     = "linkerdev-sshd"
	sshdUser     = "dev"
	sshdNodePort = int32(32022)
	linkSecret   = "linkerdev-sshd-auth"
	localKeyDir  = ".linkerdev"
	localKeyName = "id_ed25519"
)

/* ---------- Types ---------- */

type svcPort struct {
	SvcName   string
	Namespace string
	ClusterIP string
	Port      int32
	PortName  string
	LocalIP   string // unique 127.x.y.z assigned for this service
}

/* ---------- main ---------- */

func main() {
	// Check for subcommands first
	if len(os.Args) > 1 && os.Args[1] == "install" {
		cfg := loadKubeConfigOrDie()
		cs := kubernetes.NewForConfigOrDie(cfg)
		if err := installClusterSSHD(context.Background(), cs); err != nil {
			log.Fatalf("install failed: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "uninstall-sshd" {
		cfg := loadKubeConfigOrDie()
		cs := kubernetes.NewForConfigOrDie(cfg)
		if err := uninstallClusterSSHD(context.Background(), cs); err != nil {
			log.Fatalf("uninstall failed: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "dns-install" {
		installDNSResolver()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "dns-uninstall" {
		uninstallDNSResolver()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "clean" {
		cleanupResources()
		return
	}

	flagSvc := flag.String("svc", "", "Service to divert (format: name.ns)")
	flagPort := flag.Int("p", 0, "Local port your app listens on")
	flag.Parse()

	// Check DNS resolver first, before doing any Kubernetes operations
	if runtime.GOOS == "darwin" {
		ok, _ := installMacResolver()
		if !ok {
			log.Println("❌ DNS resolver not configured.")
			log.Println("   Please run: sudo linkerdev install")
			log.Fatal("Exiting until resolver is installed.")
		}
	}

	// no `--` support
	for _, a := range os.Args {
		if a == "--" {
			log.Fatalf("`--` is not supported. Usage: linkerdev -svc name.ns -p <port> <your command> [args...]")
		}
	}

	childCmd := flag.Args()
	if *flagSvc == "" || *flagPort == 0 || len(childCmd) == 0 {
		log.Fatalf("usage: linkerdev -svc name.ns -p <port> <your command> [args...]\n       linkerdev install        (install cluster SSH daemon)\n       linkerdev uninstall-sshd (remove cluster SSH daemon)\n       linkerdev dns-install    (install DNS resolver)\n       linkerdev dns-uninstall  (remove DNS resolver)\n       linkerdev clean          (clean up leftover resources)")
	}

	parts := strings.Split(*flagSvc, ".")
	if len(parts) < 2 {
		log.Fatalf("svc must be name.ns")
	}
	targetSvc, targetNS := parts[0], parts[1]
	devSvc := targetSvc + "-dev"
	remoteRPort := remotePortFor(targetSvc, targetNS) // unique per target
	instance := instanceID()
	leaseName := "linkerdev-" + targetSvc

	// Get node IP (minikube ip) and ensure we can SSH to it
	nodeIP, err := getMinikubeIP()
	if err != nil {
		log.Fatalf("minikube ip: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Kube clients
	cfg := loadKubeConfigOrDie()
	cs := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)

	// Preflight: reap stale from previous crash & acquire Lease
	must(reapStale(ctx, cs, dyn, targetNS, targetSvc, leaseName))
	lease := mustLease(ensureLease(ctx, cs, targetNS, leaseName, instance))
	stopRenew := make(chan struct{})
	go renewLeaseLoop(cs, targetNS, leaseName, instance, stopRenew)
	defer close(stopRenew)

	// Create/refresh dev Service + EndpointSlice (EndpointSlice use **nodeIP**, not loopback)
	must(ensureDevService(ctx, cs, targetNS, devSvc, int32(*flagPort), int32(remoteRPort), lease, instance))
	must(ensureDevEndpointSlice(ctx, cs, targetNS, devSvc, nodeIP, int32(remoteRPort), lease, instance))

	// Install Linkerd dynamic request routing (HTTP + gRPC) to devSvc
	// Assumes your original Service's externally exposed port equals your local port.
	servicePort := int32(*flagPort)
	must(applyHTTPRoute(ctx, dyn, targetNS, targetSvc, devSvc, servicePort, lease, instance))
	// GRPCRoute is optional - requires Gateway to be present
	if err := applyGRPCRoute(ctx, dyn, targetNS, targetSvc, devSvc, servicePort, lease, instance); err != nil {
		log.Printf("Warning: Could not create GRPCRoute (Gateway may not be configured): %v", err)
	}

	// Build inventory of all Services → DNS records + per-port forwards
	all, err := cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	must(err)
	records := map[string][]svcPort{} // fqdn -> ports
	for _, s := range all.Items {
		if s.Spec.ClusterIP == "" || s.Spec.ClusterIP == "None" {
			continue
		}
		lp := uniqueLoopbackFor(s.Name, s.Namespace)
		for _, p := range s.Spec.Ports {
			sp := svcPort{
				SvcName:   s.Name,
				Namespace: s.Namespace,
				ClusterIP: s.Spec.ClusterIP,
				Port:      p.Port,
				PortName:  p.Name,
				LocalIP:   lp,
			}
			fqdn := fmt.Sprintf("%s.%s.svc.cluster.local.", s.Name, s.Namespace)
			records[fqdn] = append(records[fqdn], sp)
		}
	}

	// Leader/follower: only one process owns DNS + -L forwards
	var dnsListen string
	if runtime.GOOS == "linux" {
		dnsListen = "127.0.0.1:53"
	} else {
		dnsListen = "127.0.0.1:1053"
	}
	leader := canBind(dnsListen)

	var dnsSrv *dns.Server
	var resolverCleanup func()

	if leader {
		dnsSrv = startDNSServer(dnsListen, records)
		defer dnsSrv.Shutdown()
		if runtime.GOOS == "darwin" {
			// DNS resolver already checked at startup, just get cleanup function
			_, cleanup := installMacResolver()
			resolverCleanup = cleanup
			defer func() {
				if resolverCleanup != nil {
					resolverCleanup()
				}
			}()
		} else {
			// Linux: ensure we have privileges to bind :53 or tell user to setcap
			if strings.HasSuffix(dnsListen, ":53") {
				// If we got here, bind succeeded; good.
			}
		}
	} else {
		log.Printf("Detected an existing linkerdev DNS at %s — running in follower mode (no -L forwards, no resolver changes).", dnsListen)
	}

	// Build SSH command:
	// -R <nodeIP>:remoteRPort:localhost:<localPort>  (cluster → laptop)
	// If leader: add -L <loopback>:port:ClusterIP:port for ALL service ports (laptop → cluster)

	// Prefer the in-cluster sshd NodePort 3022 if present
	sshPort := "22"
	if svc, err := cs.CoreV1().Services(sshdNS).Get(context.Background(), sshdName, metav1.GetOptions{}); err == nil && svc.Spec.Type == corev1.ServiceTypeNodePort {
		sshPort = fmt.Sprintf("%d", sshdNodePort)
	}

	// Ensure we have the private key we generated at install
	home, _ := os.UserHomeDir()
	privKey := filepath.Join(home, localKeyDir, localKeyName)

	// ssh -i ~/.linkerdev/id_ed25519 -p <port> -N -R <nodeIP>:remoteRPort:localhost:<localPort> dev@<nodeIP>
	args := strings.Fields(sshArgs)
	args = append(args, "-i", privKey, "-p", sshPort, "-N",
		"-R", fmt.Sprintf("%s:%d:localhost:%d", nodeIP, remoteRPort, *flagPort),
	)
	if leader {
		for _, arr := range records {
			for _, sp := range arr {
				args = append(args, "-L", fmt.Sprintf("%s:%d:%s:%d", sp.LocalIP, sp.Port, sp.ClusterIP, sp.Port))
			}
		}
	}
	args = append(args, fmt.Sprintf("%s@%s", sshdUser, nodeIP))

	sshCmd := exec.Command(sshBin, args...)
	sshOut, _ := sshCmd.StdoutPipe()
	sshErr, _ := sshCmd.StderrPipe()
	if err := sshCmd.Start(); err != nil {
		_ = cs.CoordinationV1().Leases(targetNS).Delete(context.Background(), leaseName, metav1.DeleteOptions{})
		log.Fatalf("ssh start: %v", err)
	}
	go io.Copy(os.Stdout, sshOut)
	go io.Copy(os.Stderr, sshErr)
	defer func() { _ = sshCmd.Process.Kill(); _, _ = sshCmd.Process.Wait() }()

	// Run your app
	child := exec.CommandContext(ctx, childCmd[0], childCmd[1:]...)
	child.Stdout, child.Stderr, child.Stdin = os.Stdout, os.Stderr, os.Stdin

	if runtime.GOOS == "linux" {
		// On Linux we prefer scoping /etc/resolv.conf to the child only (even if we're a follower)
		// so that the child points to 127.0.0.1 (our DNS leader on :53).
		// If we're follower and no leader binds :53, outbound DNS won't work — but leader should exist.
		if err := runChildWithResolvLinux(ctx, child); err != nil {
			_ = cs.CoordinationV1().Leases(targetNS).Delete(context.Background(), leaseName, metav1.DeleteOptions{})
			log.Fatalf("child start: %v", err)
		}
	} else {
		// macOS: resolver file handles DNS for *.svc.cluster.local globally; run child directly
		if err := child.Start(); err != nil {
			_ = cs.CoordinationV1().Leases(targetNS).Delete(context.Background(), leaseName, metav1.DeleteOptions{})
			log.Fatalf("child start: %v", err)
		}
		go func() {
			if err := child.Wait(); err != nil {
			}
			cancel()
		}()
	}

	// Wait for signal or ssh exit
	waitCh := make(chan error, 1)
	go func() { waitCh <- sshCmd.Wait() }()
	var exitCode int
	select {
	case <-ctx.Done():
	case err := <-waitCh:
		if err != nil {
			exitCode = 1
		}
	}

	// Cleanup: delete the Lease (ownerRef GC removes dev Service/Endpoints + Routes)
	_ = cs.CoordinationV1().Leases(targetNS).Delete(context.Background(), leaseName, metav1.DeleteOptions{})
	_ = deleteHTTPRoute(context.Background(), dyn, targetNS, targetSvc+"-route")
	_ = deleteGRPCRoute(context.Background(), dyn, targetNS, targetSvc+"-grpc-route")

	time.Sleep(150 * time.Millisecond)
	os.Exit(exitCode)
}

/* ---------- DNS server ---------- */

func startDNSServer(listen string, rec map[string][]svcPort) *dns.Server {
	dns.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(r)
		for _, q := range r.Question {
			name := strings.ToLower(q.Name)
			if q.Qtype == dns.TypeA && strings.HasSuffix(name, ".svc.cluster.local.") {
				if arr, ok := rec[name]; ok && len(arr) > 0 {
					ip := net.ParseIP(arr[0].LocalIP)
					if ip != nil {
						rr := &dns.A{
							Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1},
							A:   ip,
						}
						resp.Answer = append(resp.Answer, rr)
						continue
					}
				}
			}
			resp.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(resp)
	})
	s := &dns.Server{Addr: listen, Net: "udp"}
	go func() {
		if err := s.ListenAndServe(); err != nil {
			log.Printf("dns server stop: %v", err)
		}
	}()
	return s
}

/* ---------- Leader detection ---------- */

func canBind(addr string) bool {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	_ = pc.Close()
	return true
}

/* ---------- macOS resolver helpers ---------- */

func installDNSResolver() {
	if runtime.GOOS != "darwin" {
		log.Println("❌ DNS resolver installation is only supported on macOS")
		return
	}

	log.Println("🔧 Installing DNS resolver for linkerdev...")

	// Check if file already exists with correct content
	content := "nameserver 127.0.0.1\nport 1053\n"
	if existingContent, err := os.ReadFile(resolverFile); err == nil {
		if string(existingContent) == content {
			log.Println("✅ DNS resolver already installed and configured correctly")
			return
		}
		log.Printf("⚠️  DNS resolver exists but has different content. Current content:\n%s", string(existingContent))
	}

	// Try to create the directory
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		log.Printf("❌ Failed to create directory %s: %v", resolverDir, err)
		log.Println("❌ Permission denied. Please run with sudo:")
		log.Println("   sudo linkerdev install")
		return
	}

	// Try to write the file
	if err := os.WriteFile(resolverFile, []byte(content), 0o644); err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) {
			log.Println("❌ Permission denied. Please run with sudo:")
			log.Println("   sudo linkerdev install")
			return
		}
		log.Printf("❌ Failed to write resolver file: %v", err)
		return
	}

	log.Println("✅ DNS resolver installed successfully!")
	log.Println("🎉 You can now run linkerdev without additional setup")
}

func uninstallDNSResolver() {
	if runtime.GOOS != "darwin" {
		log.Println("❌ DNS resolver uninstallation is only supported on macOS")
		return
	}

	log.Println("🗑️  Uninstalling DNS resolver for linkerdev...")

	// Check if file exists
	if _, err := os.Stat(resolverFile); os.IsNotExist(err) {
		log.Println("✅ DNS resolver file does not exist - nothing to uninstall")
		return
	}

	// Try to remove the file
	if err := os.Remove(resolverFile); err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) {
			log.Println("❌ Permission denied. Please run with sudo:")
			log.Println("   sudo linkerdev uninstall")
			return
		}
		log.Printf("❌ Failed to remove resolver file: %v", err)
		return
	}

	log.Println("✅ DNS resolver uninstalled successfully!")
	log.Println("ℹ️  You can reinstall it anytime with: linkerdev install")
}

/* ---------- Cluster SSH daemon helpers ---------- */

func installClusterSSHD(ctx context.Context, cs *kubernetes.Clientset) error {
	// 1) Ensure keypair locally
	home, _ := os.UserHomeDir()
	keyDir := filepath.Join(home, localKeyDir)
	priv := filepath.Join(keyDir, localKeyName)
	pub := priv + ".pub"
	if _, err := os.Stat(priv); os.IsNotExist(err) {
		if err := os.MkdirAll(keyDir, 0o700); err != nil {
			return err
		}
		// generate ed25519 key
		cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-C", "linkerdev")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ssh-keygen: %w", err)
		}
	}
	pubBytes, err := os.ReadFile(pub)
	if err != nil {
		return fmt.Errorf("read pub key: %w", err)
	}

	// 2) Secret with authorized_keys
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      linkSecret,
			Namespace: sshdNS,
			Labels:    map[string]string{kdvLabelOwned: "true"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"authorized_keys": pubBytes,
			"username":        []byte(sshdUser),
		},
	}
	sc := cs.CoreV1().Secrets(sshdNS)
	if _, err := sc.Get(ctx, linkSecret, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := sc.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err == nil {
		sec.ResourceVersion = "" // force replace
		if _, err := sc.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			return err
		}
	} else {
		return err
	}

	// 3) Deployment running sshd (alpine openssh)
	// Note: image chosen for ubiquity; adjust if you have a preferred internal image.
	replicas := int32(1)
	dep := &appsV1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sshdName,
			Namespace: sshdNS,
			Labels:    map[string]string{kdvLabelOwned: "true", "app": sshdName},
		},
		Spec: appsV1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": sshdName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": sshdName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "sshd",
						Image:   "alpine:3.20",
						Command: []string{"/bin/sh", "-lc"},
						Args: []string{
							// install openssh-server, create user, install authorized_keys, run sshd in foreground
							`apk add --no-cache openssh-server &&
                             ssh-keygen -A &&
                             adduser -D -s /bin/sh ` + sshdUser + ` &&
                             mkdir -p /home/` + sshdUser + `/.ssh &&
                             cp /auth/authorized_keys /home/` + sshdUser + `/.ssh/authorized_keys &&
                             chown -R ` + sshdUser + `:` + sshdUser + ` /home/` + sshdUser + `/.ssh &&
                             chmod 700 /home/` + sshdUser + `/.ssh &&
                             chmod 600 /home/` + sshdUser + `/.ssh/authorized_keys &&
                             sed -i 's/#Port 22/Port 22/' /etc/ssh/sshd_config &&
                             sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config &&
                             sed -i 's/#PubkeyAuthentication yes/PubkeyAuthentication yes/' /etc/ssh/sshd_config &&
                             echo 'AllowUsers ` + sshdUser + `' >> /etc/ssh/sshd_config &&
                             /usr/sbin/sshd -D -e`,
						},
						Ports:        []corev1.ContainerPort{{Name: "ssh", ContainerPort: 22}},
						VolumeMounts: []corev1.VolumeMount{{Name: "auth", MountPath: "/auth", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{
						Name: "auth",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: linkSecret},
						},
					}},
				},
			},
		},
	}
	dc := cs.AppsV1().Deployments(sshdNS)
	if _, err := dc.Get(ctx, sshdName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := dc.Create(ctx, dep, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err == nil {
		if _, err := dc.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return err
		}
	} else {
		return err
	}

	// 4) Service (NodePort 3022)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sshdName,
			Namespace: sshdNS,
			Labels:    map[string]string{kdvLabelOwned: "true"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app": sshdName},
			Ports: []corev1.ServicePort{{
				Name:       "ssh",
				Port:       22,
				TargetPort: intstrutil.FromInt(22),
				NodePort:   sshdNodePort,
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	svcCli := cs.CoreV1().Services(sshdNS)
	if _, err := svcCli.Get(ctx, sshdName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := svcCli.Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err == nil {
		if _, err := svcCli.Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
			return err
		}
	} else {
		return err
	}

	// 5) (Optional) Restrict NodePort with NetworkPolicy (cluster must enforce Calico/Cilium etc.)
	// Skipped by default — add if needed.

	log.Println("✅ Installed linkerdev sshd NodePort on 32022 (namespace kube-system)")
	return nil
}

func uninstallClusterSSHD(ctx context.Context, cs *kubernetes.Clientset) error {
	var firstErr error
	if err := cs.CoreV1().Services(sshdNS).Delete(ctx, sshdName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		firstErr = err
	}
	if err := cs.AppsV1().Deployments(sshdNS).Delete(ctx, sshdName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		firstErr = err
	}
	if err := cs.CoreV1().Secrets(sshdNS).Delete(ctx, linkSecret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		firstErr = err
	}
	log.Println("🗑️  Uninstalled linkerdev sshd")
	return firstErr
}

func cleanupResources() {
	log.Println("🧹 Cleaning up linkerdev resources...")

	// Load kube config
	cfg := loadKubeConfigOrDie()
	cs := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)

	// Get all namespaces
	namespaces, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Printf("❌ Failed to list namespaces: %v", err)
		return
	}

	cleanedCount := 0

	// Clean up resources in each namespace
	for _, ns := range namespaces.Items {
		nsName := ns.Name

		// Find all linkerdev leases
		leases, err := cs.CoordinationV1().Leases(nsName).List(context.Background(), metav1.ListOptions{
			LabelSelector: kdvLabelOwned + "=true",
		})
		if err != nil {
			continue // Skip namespaces we can't access
		}

		for _, lease := range leases.Items {
			if strings.HasPrefix(lease.Name, "linkerdev-") {
				log.Printf("🗑️  Cleaning up lease %s/%s", nsName, lease.Name)

				// Delete the lease (this will trigger garbage collection of owned resources)
				err := cs.CoordinationV1().Leases(nsName).Delete(context.Background(), lease.Name, metav1.DeleteOptions{})
				if err != nil {
					log.Printf("⚠️  Failed to delete lease %s/%s: %v", nsName, lease.Name, err)
				} else {
					cleanedCount++
				}
			}
		}

		// Also clean up any orphaned resources with our labels
		sel := metav1.ListOptions{LabelSelector: kdvLabelOwned + "=true"}

		// Clean up Services
		services, err := cs.CoreV1().Services(nsName).List(context.Background(), sel)
		if err == nil {
			for _, svc := range services.Items {
				if strings.HasSuffix(svc.Name, "-dev") {
					log.Printf("🗑️  Cleaning up service %s/%s", nsName, svc.Name)
					err := cs.CoreV1().Services(nsName).Delete(context.Background(), svc.Name, metav1.DeleteOptions{})
					if err != nil {
						log.Printf("⚠️  Failed to delete service %s/%s: %v", nsName, svc.Name, err)
					} else {
						cleanedCount++
					}
				}
			}
		}

		// Clean up EndpointSlices
		endpointSlices, err := cs.DiscoveryV1().EndpointSlices(nsName).List(context.Background(), sel)
		if err == nil {
			for _, es := range endpointSlices.Items {
				if strings.Contains(es.Name, "-dev-") {
					log.Printf("🗑️  Cleaning up endpointslice %s/%s", nsName, es.Name)
					err := cs.DiscoveryV1().EndpointSlices(nsName).Delete(context.Background(), es.Name, metav1.DeleteOptions{})
					if err != nil {
						log.Printf("⚠️  Failed to delete endpointslice %s/%s: %v", nsName, es.Name, err)
					} else {
						cleanedCount++
					}
				}
			}
		}

		// Clean up HTTPRoutes
		httproutes, err := dyn.Resource(schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "httproutes"}).Namespace(nsName).List(context.Background(), metav1.ListOptions{LabelSelector: kdvLabelOwned + "=true"})
		if err == nil {
			for _, item := range httproutes.Items {
				name := item.GetName()
				if strings.HasSuffix(name, "-route") {
					log.Printf("🗑️  Cleaning up HTTPRoute %s/%s", nsName, name)
					err := dyn.Resource(schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "httproutes"}).Namespace(nsName).Delete(context.Background(), name, metav1.DeleteOptions{})
					if err != nil {
						log.Printf("⚠️  Failed to delete HTTPRoute %s/%s: %v", nsName, name, err)
					} else {
						cleanedCount++
					}
				}
			}
		}

		// Clean up GRPCRoutes
		grpcroutes, err := dyn.Resource(schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"}).Namespace(nsName).List(context.Background(), metav1.ListOptions{LabelSelector: kdvLabelOwned + "=true"})
		if err == nil {
			for _, item := range grpcroutes.Items {
				name := item.GetName()
				if strings.HasSuffix(name, "-grpc-route") {
					log.Printf("🗑️  Cleaning up GRPCRoute %s/%s", nsName, name)
					err := dyn.Resource(schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"}).Namespace(nsName).Delete(context.Background(), name, metav1.DeleteOptions{})
					if err != nil {
						log.Printf("⚠️  Failed to delete GRPCRoute %s/%s: %v", nsName, name, err)
					} else {
						cleanedCount++
					}
				}
			}
		}
	}

	if cleanedCount > 0 {
		log.Printf("✅ Cleaned up %d linkerdev resources", cleanedCount)
	} else {
		log.Println("✅ No linkerdev resources found to clean up")
	}
}

func installMacResolver() (bool, func()) {
	content := "nameserver 127.0.0.1\nport 1053\n"

	// Check if file already exists with correct content
	if existingContent, err := os.ReadFile(resolverFile); err == nil {
		if string(existingContent) == content {
			cleanup := func() { _ = os.Remove(resolverFile) }
			return true, cleanup
		}
	}

	// File doesn't exist or has wrong content - need to install
	return false, nil
}

/* ---------- Linux child with private resolv.conf ---------- */

func runChildWithResolvLinux(ctx context.Context, child *exec.Cmd) error {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("linkerdev-%d", os.Getpid()))
	_ = os.MkdirAll(tmp, 0o700)
	resolv := filepath.Join(tmp, "resolv.conf")
	content := "nameserver 127.0.0.1\noptions timeout:1 attempts:1 ndots:5\n"
	if err := os.WriteFile(resolv, []byte(content), 0o644); err != nil {
		return err
	}
	args := []string{
		"--mount", "--uts", "--ipc",
		"--propagation", "private",
		"/bin/sh", "-lc",
		fmt.Sprintf("mount --make-rprivate / && mount -o bind %s /etc/resolv.conf && exec \"$@\"", shQuote(resolv)),
		"sh", "-lc", strings.Join(append([]string{child.Path}, child.Args[1:]...), " "),
	}
	cmd := exec.CommandContext(ctx, "unshare", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

/* ---------- K8s: HTTPRoute / GRPCRoute (Linkerd) ---------- */

func applyHTTPRoute(ctx context.Context, dyn dynamic.Interface, ns, svc, devSvc string, port int32, lease *coordv1.Lease, instance string) error {
	gvr := schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "httproutes"}
	name := svc + "-route"
	obj := map[string]any{
		"apiVersion": "policy.linkerd.io/v1beta3",
		"kind":       "HTTPRoute",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels": map[string]string{
				kdvLabelOwned: "true",
				kdvLabelInst:  instance,
			},
			"ownerReferences": []map[string]any{{
				"apiVersion":         "coordination.k8s.io/v1",
				"kind":               "Lease",
				"name":               lease.Name,
				"uid":                string(lease.UID),
				"controller":         true,
				"blockOwnerDeletion": true,
			}},
		},
		"spec": map[string]any{
			"parentRefs": []map[string]any{
				{"name": svc, "kind": "Service", "group": "core", "port": port},
			},
			"rules": []map[string]any{
				{
					"backendRefs": []map[string]any{
						{"name": devSvc, "port": port},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(obj)
	_, err := dyn.Resource(gvr).Namespace(ns).Patch(ctx, name, types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: "linkerdev"})
	return err
}

func deleteHTTPRoute(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	gvr := schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "httproutes"}
	return dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

func applyGRPCRoute(ctx context.Context, dyn dynamic.Interface, ns, svc, devSvc string, port int32, lease *coordv1.Lease, instance string) error {
	gvr := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"}
	name := svc + "-grpc-route"
	obj := map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "GRPCRoute",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels": map[string]string{
				kdvLabelOwned: "true",
				kdvLabelInst:  instance,
			},
			"ownerReferences": []map[string]any{{
				"apiVersion":         "coordination.k8s.io/v1",
				"kind":               "Lease",
				"name":               lease.Name,
				"uid":                string(lease.UID),
				"controller":         true,
				"blockOwnerDeletion": true,
			}},
		},
		"spec": map[string]any{
			"parentRefs": []map[string]any{
				{"name": svc, "kind": "Service", "group": "core", "port": port},
			},
			"rules": []map[string]any{
				{
					"backendRefs": []map[string]any{
						{"name": devSvc, "port": port},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(obj)
	_, err := dyn.Resource(gvr).Namespace(ns).Patch(ctx, name, types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: "linkerdev"})
	return err
}

func deleteGRPCRoute(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	gvr := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"}
	return dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

/* ---------- K8s: dev Service/Endpoints + Lease ---------- */

func loadKubeConfigOrDie() *rest.Config {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		log.Fatalf("kubeconfig: %v", err)
	}
	return cfg
}

func ensureDevService(ctx context.Context, cs *kubernetes.Clientset, ns, name string, svcPort, remotePort int32, lease *coordv1.Lease, instance string) error {
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:          map[string]string{kdvLabelOwned: "true", kdvLabelInst: instance},
			OwnerReferences: ownerRefToLease(lease),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       svcPort,
				TargetPort: intstrutil.FromInt(int(remotePort)),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if _, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		existing, _ := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		existing.Labels = s.Labels
		existing.OwnerReferences = s.OwnerReferences
		existing.Spec.Ports = s.Spec.Ports
		_, err := cs.CoreV1().Services(ns).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}
	_, err := cs.CoreV1().Services(ns).Create(ctx, s, metav1.CreateOptions{})
	return err
}

func ptr[T any](v T) *T { return &v }

func ensureDevEndpointSlice(ctx context.Context, cs *kubernetes.Clientset, ns, svcName, ip string, port int32, lease *coordv1.Lease, instance string) error {
	// Name must be unique per namespace; tie to service + instance
	esName := fmt.Sprintf("%s-%s", svcName, instance)

	addrType := discoveryv1.AddressTypeIPv4
	ready := true
	svcLabel := map[string]string{
		"kubernetes.io/service-name": svcName, // required: associates slice to Service
		kdvLabelOwned:                "true",
		kdvLabelInst:                 instance,
	}

	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            esName,
			Namespace:       ns,
			Labels:          svcLabel,
			OwnerReferences: ownerRefToLease(lease),
		},
		AddressType: addrType,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses: []string{ip},
				Conditions: discoveryv1.EndpointConditions{
					Ready: ptr(ready),
				},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     ptr("http"),
				Protocol: ptr(corev1.ProtocolTCP),
				Port:     ptr(port),
			},
		},
	}

	// Upsert by name
	existing, err := cs.DiscoveryV1().EndpointSlices(ns).Get(ctx, esName, metav1.GetOptions{})
	if err == nil {
		// update fields that may change
		existing.Labels = es.Labels
		existing.OwnerReferences = es.OwnerReferences
		existing.AddressType = es.AddressType
		existing.Endpoints = es.Endpoints
		existing.Ports = es.Ports
		_, err = cs.DiscoveryV1().EndpointSlices(ns).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}

	_, err = cs.DiscoveryV1().EndpointSlices(ns).Create(ctx, es, metav1.CreateOptions{})
	return err
}

func applyNoop(_ context.Context) error { return nil } // placeholder if needed

func instanceID() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", h, os.Getpid())
}

func ownerRefToLease(lease *coordv1.Lease) []metav1.OwnerReference {
	t := true
	return []metav1.OwnerReference{{
		APIVersion:         "coordination.k8s.io/v1",
		Kind:               "Lease",
		Name:               lease.Name,
		UID:                lease.UID,
		Controller:         &t,
		BlockOwnerDeletion: &t,
	}}
}

func ensureLease(ctx context.Context, cs *kubernetes.Clientset, ns, name, holder string) (*coordv1.Lease, error) {
	now := metav1.MicroTime{Time: time.Now()}
	secs := int32(int(leaseStaleAfter.Seconds()))
	l := &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coordv1.LeaseSpec{
			HolderIdentity:       &holder,
			AcquireTime:          &now,
			RenewTime:            &now,
			LeaseDurationSeconds: &secs,
		},
	}
	lg := cs.CoordinationV1().Leases(ns)
	if existing, err := lg.Get(ctx, name, metav1.GetOptions{}); err == nil {
		existing.Spec.HolderIdentity = &holder
		existing.Spec.RenewTime = &now
		existing.Spec.AcquireTime = &now
		existing.Spec.LeaseDurationSeconds = &secs
		return lg.Update(ctx, existing, metav1.UpdateOptions{})
	}
	return lg.Create(ctx, l, metav1.CreateOptions{})
}

func renewLeaseLoop(cs *kubernetes.Clientset, ns, name, holder string, stop <-chan struct{}) {
	lg := cs.CoordinationV1().Leases(ns)
	t := time.NewTicker(leaseRenewEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l, err := lg.Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			now := metav1.MicroTime{Time: time.Now()}
			l.Spec.RenewTime = &now
			l.Spec.HolderIdentity = &holder
			_, _ = lg.Update(context.Background(), l, metav1.UpdateOptions{})
		case <-stop:
			return
		}
	}
}

func reapStale(ctx context.Context, cs *kubernetes.Clientset, dyn dynamic.Interface, ns, svc, leaseName string) error {
	lg := cs.CoordinationV1().Leases(ns)
	lease, err := lg.Get(ctx, leaseName, metav1.GetOptions{})
	if err == nil {
		if isLeaseStale(lease) {
			log.Printf("[linkerdev] found stale lease %s/%s — deleting & sweeping labeled resources", ns, leaseName)
			_ = lg.Delete(ctx, leaseName, metav1.DeleteOptions{})

			sel := metav1.ListOptions{LabelSelector: kdvLabelOwned + "=true"}

			// Delete Services individually (DeleteCollection not available)
			services, err := cs.CoreV1().Services(ns).List(ctx, sel)
			if err == nil {
				for _, svc := range services.Items {
					_ = cs.CoreV1().Services(ns).Delete(ctx, svc.Name, metav1.DeleteOptions{})
				}
			}

			// Delete EndpointSlices we created (labelled)
			esList, err := cs.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
				LabelSelector: kdvLabelOwned + "=true",
			})
			if err == nil {
				for _, es := range esList.Items {
					_ = cs.DiscoveryV1().EndpointSlices(ns).Delete(ctx, es.Name, metav1.DeleteOptions{})
				}
			}
			log.Printf("Deleting HTTPRoute %s/%s-route", ns, svc)
			if err := deleteHTTPRoute(ctx, dyn, ns, svc+"-route"); err != nil {
				log.Printf("Warning: Could not delete HTTPRoute: %v", err)
			}
			log.Printf("Deleting GRPCRoute %s/%s-grpc-route", ns, svc)
			if err := deleteGRPCRoute(ctx, dyn, ns, svc+"-grpc-route"); err != nil {
				log.Printf("Warning: Could not delete GRPCRoute: %v", err)
			}
		} else {
			return fmt.Errorf("another linkerdev instance appears active (lease %s/%s is fresh)", ns, leaseName)
		}
	}
	return nil
}

func isLeaseStale(l *coordv1.Lease) bool {
	if l.Spec.RenewTime != nil {
		return time.Since(l.Spec.RenewTime.Time) > leaseStaleAfter
	}
	if l.Spec.AcquireTime != nil {
		return time.Since(l.Spec.AcquireTime.Time) > leaseStaleAfter
	}
	return true
}

func mustLease(l *coordv1.Lease, err error) *coordv1.Lease {
	if err != nil {
		log.Fatal(err)
	}
	return l
}

/* ---------- misc helpers ---------- */

func remotePortFor(svc, ns string) int {
	h := sha1.Sum([]byte(ns + "/" + svc))
	n := binary.BigEndian.Uint16(h[:2])
	return 20000 + int(n%10000) // 20000..29999
}

func uniqueLoopbackFor(svc, ns string) string {
	h := sha1.Sum([]byte(ns + "/" + svc))
	a := 10
	b := int(h[0])
	c := int(h[1])
	return fmt.Sprintf("127.%d.%d.%d", a, b, c)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func getMinikubeIP() (string, error) {
	cmd := exec.Command("minikube", "ip")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run minikube ip: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
