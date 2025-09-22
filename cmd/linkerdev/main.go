// linkerdev.go — Telepresence-lite via in-cluster relay (Docker-driver friendly)
// Build: go build -o linkerdev .
// Usage: linkerdev -svc <name.ns> -p <local-port> <your app cmd> [args...]
// Commands:
//   linkerdev install         (install relay component in cluster)
//   linkerdev uninstall       (remove relay component from cluster)
//   linkerdev install-dns     (macOS only; one-time /etc/resolver setup)
//   linkerdev uninstall-dns   (macOS only; remove resolver file)
//   linkerdev clean           (best-effort sweep of leftover resources)
//   linkerdev version         (show version information)
//   linkerdev help            (show help message)

package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

// Version is set at build time for releases
var version = "v0.1.0-alpha10"

const (
	// Labels
	kdvLabelOwned = "app.kdvwrap/owned"
	kdvLabelInst  = "app.kdvwrap/instance"

	// Lease/cleanup
	leaseRenewEvery = 10 * time.Second
	leaseStaleAfter = 2 * time.Minute

	// macOS resolver
	resolverDir  = "/etc/resolver"
	resolverFile = "/etc/resolver/svc.cluster.local"

	// Relay (in-cluster)
	relayName = "linkerdev-relay"
	relayCtrl = 18080 // control port inside the pod
)

var relayImg = "ghcr.io/sopatech/linkerdev-relay:" + version

/* ---------- main ---------- */

func main() {
	// Subcommands: relay install/uninstall + DNS resolver (mac) + cleanup + version + help
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			installRelay()
			return
		case "uninstall":
			uninstallRelay()
			return
		case "install-dns":
			installDNSResolver()
			return
		case "uninstall-dns":
			uninstallDNSResolver()
			return
		case "clean":
			cleanupResources()
			return
		case "version":
			fmt.Println(version)
			return
		case "help":
			showHelp()
			return
		}
	}

	flagSvc := flag.String("svc", "", "Service to divert (format: name.ns)")
	flagPort := flag.Int("p", 0, "Local port your app listens on")
	flag.Parse()

	// For simplicity, we don't support `--` separator
	for _, a := range os.Args {
		if a == "--" {
			log.Fatalf("`--` is not supported. Usage: linkerdev -svc name.ns -p <port> <your command> [args...]")
		}
	}
	childCmd := flag.Args()
	if *flagSvc == "" || *flagPort == 0 || len(childCmd) == 0 {
		showHelp()
		os.Exit(1)
	}

	// macOS: the DNS resolver is only needed if you later enable transparent outbound;
	// we don't require it here to run the inbound flow.
	// If you want DNS-based outbound later: run `sudo linkerdev install-dns`.

	// Parse target
	parts := strings.Split(*flagSvc, ".")
	if len(parts) < 2 {
		log.Fatalf("svc must be name.ns")
	}
	targetSvc, targetNS := parts[0], parts[1]

	// Validate service name and namespace
	if targetSvc == "" || targetNS == "" {
		log.Fatalf("service name and namespace cannot be empty")
	}
	if !isValidKubernetesName(targetSvc) || !isValidKubernetesName(targetNS) {
		log.Fatalf("service name and namespace must be valid Kubernetes names (alphanumeric and hyphens only)")
	}
	devSvc := targetSvc + "-dev"
	remoteRPort := int32(remotePortFor(targetSvc, targetNS)) // relay listen port in cluster
	instance := instanceID()
	leaseName := "linkerdev-" + targetSvc

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Kube clients
	cfg := loadKubeConfigOrDie()
	cs := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)

	// Preflight: sweep stale & acquire Lease
	must(reapStale(ctx, cs, dyn, targetNS, targetSvc, leaseName))
	lease := mustLease(ensureLease(ctx, cs, targetNS, leaseName, instance))
	stopRenew := make(chan struct{})
	go renewLeaseLoop(cs, targetNS, leaseName, instance, stopRenew)
	defer close(stopRenew)

	// Ensure relay (Deployment + ClusterIP Service) and get its Pod IP
	podIP, err := ensureRelay(ctx, cs, targetNS, remoteRPort, lease, instance)
	must(err)

	// Create/refresh dev Service + EndpointSlice that points to RELAY POD IP:remoteRPort
	must(ensureDevService(ctx, cs, targetNS, devSvc, int32(*flagPort), remoteRPort, lease, instance))
	must(ensureDevEndpointSlice(ctx, cs, targetNS, devSvc, podIP, remoteRPort, lease, instance))

	// Install Linkerd dynamic routing (HTTP + gRPC) to devSvc (best-effort)
	servicePort := int32(*flagPort)
	if err := applyHTTPRoute(ctx, dyn, targetNS, targetSvc, devSvc, servicePort, lease, instance); err != nil {
		log.Printf("HTTPRoute apply warning: %v", err)
	}
	if err := applyGRPCRoute(ctx, dyn, targetNS, targetSvc, devSvc, servicePort, lease, instance); err != nil {
		log.Printf("GRPCRoute apply warning: %v", err)
	}

	// kubectl port-forward: forward relay control port to a free local port
	localCtrl, err := randomFreePort()
	must(err)
	kpf, err := startKubectlPortForward(ctx, targetNS, relayName, localCtrl, relayCtrl)
	must(err)
	defer func() {
		if kpf.Process != nil {
			_ = kpf.Process.Kill()
			// Give the process a moment to exit gracefully
			done := make(chan error, 1)
			go func() { _, err := kpf.Process.Wait(); done <- err }()
			select {
			case <-done:
				// Process exited
			case <-time.After(2 * time.Second):
				// Force kill if it doesn't exit gracefully
				_ = kpf.Process.Kill()
			}
		}
	}()

	// Open a control connection to the relay and start multiplexing cluster->local
	rc, err := newRelayClient(fmt.Sprintf("127.0.0.1:%d", localCtrl))
	must(err)
	go rc.loop(*flagPort) // each inbound stream connects to 127.0.0.1:<local port>

	// Optional: start DNS if you later want transparent outbound (kept off by default)
	_ = startOptionalDNS(recordsForAllServices(ctx, cs))

	// Run your app
	child := exec.CommandContext(ctx, childCmd[0], childCmd[1:]...)
	child.Stdout, child.Stderr, child.Stdin = os.Stdout, os.Stderr, os.Stdin

	// Ensure lease cleanup happens regardless of how child exits
	defer func() {
		_ = cs.CoordinationV1().Leases(targetNS).Delete(context.Background(), leaseName, metav1.DeleteOptions{})
	}()

	if runtime.GOOS == "linux" {
		// Scope resolv.conf to the child (future outbound). Safe even if unused.
		if err := runChildWithResolvLinux(ctx, child); err != nil {
			log.Fatalf("child start: %v", err)
		}
	} else {
		if err := child.Start(); err != nil {
			log.Fatalf("child start: %v", err)
		}
		go func() {
			_ = child.Wait()
			cancel() // Signal that child has exited
		}()
	}

	// Wait for signal
	<-ctx.Done()

	// Cleanup (Lease ownerRef GC will remove relay svc/dep, dev svc/slice, routes)
	_ = cs.CoordinationV1().Leases(targetNS).Delete(context.Background(), leaseName, metav1.DeleteOptions{})
	_ = deleteHTTPRoute(context.Background(), dyn, targetNS, targetSvc+"-route")
	_ = deleteGRPCRoute(context.Background(), dyn, targetNS, targetSvc+"-grpc-route")
	time.Sleep(150 * time.Millisecond)
}

/* ---------- Relay client (multiplex many in-cluster streams over one control TCP) ---------- */

type relayClient struct {
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	writeMu sync.Mutex
	streams sync.Map // streamID -> net.Conn (local app connection)
}

const (
	tOpen  = 0x01
	tData  = 0x02
	tClose = 0x03
	tRst   = 0x04
	tPing  = 0x10
	tPong  = 0x11
)

func newRelayClient(addr string) (*relayClient, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	rc := &relayClient{
		conn: c,
		br:   bufio.NewReader(c),
		bw:   bufio.NewWriterSize(c, 64<<10),
	}
	_ = rc.sendFrame(tPing, 0, []byte("HELLO LINKERDEV"))
	return rc, nil
}

func (rc *relayClient) loop(localAppPort int) {
	defer rc.conn.Close()
	for {
		fr, err := readFrame(rc.br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("[relay] control read error: %v", err)
			}
			return
		}
		switch fr.typ {
		case tOpen:
			app, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localAppPort))
			if err != nil {
				_ = rc.sendFrame(tRst, fr.streamID, nil)
				continue
			}
			rc.streams.Store(fr.streamID, app)
			// pump app -> relay as DATA frames
			go func(id uint32, a net.Conn) {
				defer func() {
					_ = a.Close()
					rc.streams.Delete(id)
				}()
				buf := make([]byte, 64<<10)
				for {
					n, err := a.Read(buf)
					if n > 0 {
						if err := rc.sendFrame(tData, id, buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						if err == io.EOF {
							_ = rc.sendFrame(tClose, id, nil)
						} else {
							_ = rc.sendFrame(tRst, id, nil)
						}
						return
					}
				}
			}(fr.streamID, app)

		case tData:
			if v, ok := rc.streams.Load(fr.streamID); ok {
				a := v.(net.Conn)
				if _, err := a.Write(fr.payload); err != nil {
					_ = rc.sendFrame(tRst, fr.streamID, nil)
					_ = a.Close()
					rc.streams.Delete(fr.streamID)
				}
			} else {
				_ = rc.sendFrame(tRst, fr.streamID, nil)
			}

		case tClose:
			if v, ok := rc.streams.Load(fr.streamID); ok {
				if tcp, ok2 := v.(net.Conn).(*net.TCPConn); ok2 {
					_ = tcp.CloseWrite()
				}
			}

		case tRst:
			if v, ok := rc.streams.LoadAndDelete(fr.streamID); ok {
				_ = v.(net.Conn).Close()
			}

		case tPing:
			_ = rc.sendFrame(tPong, 0, fr.payload)
		}
	}
}

func (rc *relayClient) sendFrame(typ byte, id uint32, payload []byte) error {
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	var hdr [9]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], id)
	if payload == nil {
		binary.BigEndian.PutUint32(hdr[5:9], 0)
		if _, err := rc.bw.Write(hdr[:]); err != nil {
			return err
		}
		return rc.bw.Flush()
	}
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(payload)))
	if _, err := rc.bw.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := rc.bw.Write(payload); err != nil {
		return err
	}
	return rc.bw.Flush()
}

type frame struct {
	typ      byte
	streamID uint32
	payload  []byte
}

func readFrame(r *bufio.Reader) (*frame, error) {
	var hdr [9]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	typ := hdr[0]
	id := binary.BigEndian.Uint32(hdr[1:5])
	l := binary.BigEndian.Uint32(hdr[5:9])
	var payload []byte
	if l > 0 {
		payload = make([]byte, l)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return &frame{typ: typ, streamID: id, payload: payload}, nil
}

/* ---------- Help command ---------- */

func showHelp() {
	fmt.Println("linkerdev - Telepresence-lite via in-cluster relay")
	fmt.Println()
	fmt.Println("USAGE:")
	fmt.Println("  linkerdev -svc <service.namespace> -p <port> <command> [args...]")
	fmt.Println()
	fmt.Println("COMMANDS:")
	fmt.Println("  install         Install relay component in cluster")
	fmt.Println("  uninstall       Remove relay component from cluster")
	fmt.Println("  install-dns     Install DNS resolver (macOS only)")
	fmt.Println("  uninstall-dns   Remove DNS resolver (macOS only)")
	fmt.Println("  clean           Clean up leftover resources")
	fmt.Println("  version         Show version information")
	fmt.Println("  help            Show this help message")
	fmt.Println()
	fmt.Println("EXAMPLES:")
	fmt.Println("  # Install the relay component")
	fmt.Println("  sudo linkerdev install")
	fmt.Println()
	fmt.Println("  # Install DNS resolver (macOS only)")
	fmt.Println("  sudo linkerdev install-dns")
	fmt.Println()
	fmt.Println("  # Run your service with linkerdev")
	fmt.Println("  linkerdev -svc api-service.apps -p 8080 go run main.go")
	fmt.Println("  linkerdev -svc web-service.apps -p 3000 npm start")
	fmt.Println()
	fmt.Println("  # Clean up resources")
	fmt.Println("  linkerdev clean")
}

/* ---------- Relay install/uninstall ---------- */

func installRelay() {
	log.Println("🚀 Installing linkerdev relay component...")
	cfg := loadKubeConfigOrDie()
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()

	// Use kube-system namespace for the relay
	ns := "kube-system"

	// Create a simple lease for ownership
	lease, err := ensureLease(ctx, cs, ns, "linkerdev-relay-lease", "linkerdev-install")
	if err != nil {
		log.Printf("❌ Failed to create lease: %v", err)
		return
	}

	// Install the relay with a default port
	remotePort := int32(20000)
	instance := "install"

	_, err = ensureRelay(ctx, cs, ns, remotePort, lease, instance)
	if err != nil {
		log.Printf("❌ Failed to install relay: %v", err)
		return
	}

	log.Println("✅ linkerdev relay component installed successfully")
	log.Println("   The relay is now running in the kube-system namespace")
}

func uninstallRelay() {
	log.Println("🗑️  Uninstalling linkerdev relay component...")
	cfg := loadKubeConfigOrDie()
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()

	// Use kube-system namespace
	ns := "kube-system"

	// Delete the relay deployment
	if err := cs.AppsV1().Deployments(ns).Delete(ctx, relayName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("❌ Failed to delete deployment: %v", err)
		}
	} else {
		log.Println("✅ Deleted relay deployment")
	}

	// Delete the relay service
	if err := cs.CoreV1().Services(ns).Delete(ctx, relayName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("❌ Failed to delete service: %v", err)
		}
	} else {
		log.Println("✅ Deleted relay service")
	}

	// Delete the lease
	if err := cs.CoordinationV1().Leases(ns).Delete(ctx, "linkerdev-relay-lease", metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("❌ Failed to delete lease: %v", err)
		}
	} else {
		log.Println("✅ Deleted relay lease")
	}

	log.Println("✅ linkerdev relay component uninstalled successfully")
}

/* ---------- macOS DNS install/uninstall ---------- */

func installDNSResolver() {
	if runtime.GOOS != "darwin" {
		log.Println("❌ DNS resolver installation is only supported on macOS")
		return
	}
	content := "nameserver 127.0.0.1\nport 1053\n"
	if existingContent, err := os.ReadFile(resolverFile); err == nil && string(existingContent) == content {
		log.Println("✅ DNS resolver already installed")
		return
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		log.Printf("❌ mkdir %s: %v\n", resolverDir, err)
		log.Println("   Try: sudo linkerdev install-dns")
		return
	}
	if err := os.WriteFile(resolverFile, []byte(content), 0o644); err != nil {
		log.Println("❌ write resolver failed. Try: sudo linkerdev install-dns")
		return
	}
	log.Println("✅ DNS resolver installed")
}

func uninstallDNSResolver() {
	if runtime.GOOS != "darwin" {
		log.Println("❌ DNS resolver uninstallation is only supported on macOS")
		return
	}
	if _, err := os.Stat(resolverFile); os.IsNotExist(err) {
		log.Println("✅ No resolver file to remove")
		return
	}
	if err := os.Remove(resolverFile); err != nil {
		log.Println("❌ remove failed. Try: sudo linkerdev uninstall-dns")
		return
	}
	log.Println("✅ DNS resolver uninstalled")
}

/* ---------- Cleanup command ---------- */

func cleanupResources() {
	log.Println("🧹 Cleaning up linkerdev resources...")
	cfg := loadKubeConfigOrDie()
	cs := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)

	namespaces, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Printf("❌ list namespaces: %v", err)
		return
	}

	cleaned := 0
	for _, ns := range namespaces.Items {
		nsName := ns.Name
		sel := metav1.ListOptions{LabelSelector: kdvLabelOwned + "=true"}

		_ = cs.AppsV1().Deployments(nsName).Delete(context.Background(), relayName, metav1.DeleteOptions{})
		_ = cs.CoreV1().Services(nsName).Delete(context.Background(), relayName, metav1.DeleteOptions{})

		if svcs, err := cs.CoreV1().Services(nsName).List(context.Background(), sel); err == nil {
			for _, s := range svcs.Items {
				if strings.HasSuffix(s.Name, "-dev") {
					_ = cs.CoreV1().Services(nsName).Delete(context.Background(), s.Name, metav1.DeleteOptions{})
					cleaned++
				}
			}
		}
		if es, err := cs.DiscoveryV1().EndpointSlices(nsName).List(context.Background(), sel); err == nil {
			for _, e := range es.Items {
				_ = cs.DiscoveryV1().EndpointSlices(nsName).Delete(context.Background(), e.Name, metav1.DeleteOptions{})
				cleaned++
			}
		}
		_ = deleteHTTPRoute(context.Background(), dyn, nsName, nsName+"-route")      // best-effort; names may differ
		_ = deleteGRPCRoute(context.Background(), dyn, nsName, nsName+"-grpc-route") // best-effort
	}
	log.Printf("✅ Cleanup completed (approx %d items)", cleaned)
}

/* ---------- In-cluster relay Deployment + Service + kubectl port-forward ---------- */

func ensureRelay(ctx context.Context, cs *kubernetes.Clientset, ns string, remoteRPort int32, lease *coordv1.Lease, instance string) (string, error) {
	labels := map[string]string{"app": relayName, kdvLabelOwned: "true", kdvLabelInst: instance}
	// Deployment
	replicas := int32(1)
	dep := &appsV1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: relayName, Namespace: ns, Labels: labels, OwnerReferences: ownerRefToLease(lease)},
		Spec: appsV1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "relay",
						Image: relayImg,
						Args:  []string{"--listen", strconv.Itoa(int(remoteRPort)), "--control", strconv.Itoa(relayCtrl)},
						Ports: []corev1.ContainerPort{
							{Name: "ctrl", ContainerPort: int32(relayCtrl)},
							{Name: "rev", ContainerPort: remoteRPort},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstrutil.FromInt(relayCtrl)},
							},
							InitialDelaySeconds: 1, PeriodSeconds: 2,
						},
					}},
				},
			},
		},
	}
	dc := cs.AppsV1().Deployments(ns)
	if _, err := dc.Get(ctx, relayName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := dc.Create(ctx, dep, metav1.CreateOptions{}); err != nil {
			return "", err
		}
	} else if err == nil {
		if _, err := dc.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
	} else {
		return "", err
	}

	// Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: relayName, Namespace: ns, Labels: labels, OwnerReferences: ownerRefToLease(lease)},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "ctrl", Port: int32(relayCtrl), TargetPort: intstrutil.FromInt(relayCtrl), Protocol: corev1.ProtocolTCP},
				{Name: "rev", Port: remoteRPort, TargetPort: intstrutil.FromInt(int(remoteRPort)), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	sc := cs.CoreV1().Services(ns)
	if _, err := sc.Get(ctx, relayName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := sc.Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return "", err
		}
	} else if err == nil {
		if _, err := sc.Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
	} else {
		return "", err
	}

	// Wait for relay Pod IP
	return waitForPodIP(ctx, cs, ns, "app="+relayName+","+kdvLabelInst+"="+instance, 60*time.Second)
}

func startKubectlPortForward(ctx context.Context, ns, name string, local, remote int) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "-n", ns, "port-forward", "svc/"+name, fmt.Sprintf("%d:%d", local, remote))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	time.Sleep(700 * time.Millisecond) // settle
	return cmd, nil
}

func waitForPodIP(ctx context.Context, cs *kubernetes.Clientset, ns, selector string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("context cancelled while waiting for relay pod IP: %v", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timed out waiting for relay pod IP")
			}
			pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err == nil {
				for _, p := range pods.Items {
					if p.Status.PodIP != "" {
						return p.Status.PodIP, nil
					}
				}
			}
		}
	}
}

/* ---------- Linkerd: HTTPRoute / GRPCRoute (version-flexible) ---------- */

func applyHTTPRoute(ctx context.Context, dyn dynamic.Interface, ns, svc, devSvc string, port int32, lease *coordv1.Lease, instance string) error {
	cands := []struct {
		gvr schema.GroupVersionResource
		api string
	}{
		{schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "httproutes"}, "policy.linkerd.io/v1beta3"},
		{schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta2", Resource: "httproutes"}, "policy.linkerd.io/v1beta2"},
	}
	var last error
	for _, c := range cands {
		name := svc + "-route"
		obj := map[string]any{
			"apiVersion": c.api,
			"kind":       "HTTPRoute",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
				"labels":    map[string]string{kdvLabelOwned: "true", kdvLabelInst: instance},
				"ownerReferences": []map[string]any{{
					"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
					"name": lease.Name, "uid": string(lease.UID),
					"controller": true, "blockOwnerDeletion": true,
				}},
			},
			"spec": map[string]any{
				"parentRefs": []map[string]any{{"name": svc, "kind": "Service", "group": "core", "port": port}},
				"rules":      []map[string]any{{"backendRefs": []map[string]any{{"name": devSvc, "port": port}}}},
			},
		}
		b, _ := json.Marshal(obj)
		if _, err := dyn.Resource(c.gvr).Namespace(ns).Patch(ctx, name, types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: "linkerdev"}); err == nil {
			return nil
		} else {
			last = err
		}
	}
	return last
}

func deleteHTTPRoute(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	cands := []schema.GroupVersionResource{
		{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "httproutes"},
		{Group: "policy.linkerd.io", Version: "v1beta2", Resource: "httproutes"},
	}
	for _, gvr := range cands {
		_ = dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}
	return nil
}

func applyGRPCRoute(ctx context.Context, dyn dynamic.Interface, ns, svc, devSvc string, port int32, lease *coordv1.Lease, instance string) error {
	cands := []struct {
		gvr  schema.GroupVersionResource
		api  string
		kind string
	}{
		{schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "grpcroutes"}, "policy.linkerd.io/v1beta3", "GRPCRoute"},
		{schema.GroupVersionResource{Group: "policy.linkerd.io", Version: "v1beta2", Resource: "grpcroutes"}, "policy.linkerd.io/v1beta2", "GRPCRoute"},
		{schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"}, "gateway.networking.k8s.io/v1", "GRPCRoute"},
	}
	var last error
	for _, c := range cands {
		name := svc + "-grpc-route"
		obj := map[string]any{
			"apiVersion": c.api, "kind": c.kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
				"labels":    map[string]string{kdvLabelOwned: "true", kdvLabelInst: instance},
				"ownerReferences": []map[string]any{{
					"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
					"name": lease.Name, "uid": string(lease.UID),
					"controller": true, "blockOwnerDeletion": true,
				}},
			},
			"spec": map[string]any{
				"parentRefs": []map[string]any{{"name": svc, "kind": "Service", "group": "core", "port": port}},
				"rules":      []map[string]any{{"backendRefs": []map[string]any{{"name": devSvc, "port": port}}}},
			},
		}
		b, _ := json.Marshal(obj)
		if _, err := dyn.Resource(c.gvr).Namespace(ns).Patch(ctx, name, types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: "linkerdev"}); err == nil {
			return nil
		} else {
			last = err
		}
	}
	return last
}

func deleteGRPCRoute(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	cands := []schema.GroupVersionResource{
		{Group: "policy.linkerd.io", Version: "v1beta3", Resource: "grpcroutes"},
		{Group: "policy.linkerd.io", Version: "v1beta2", Resource: "grpcroutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"},
	}
	for _, gvr := range cands {
		_ = dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}
	return nil
}

/* ---------- K8s: dev Service/EndpointSlice + Lease ---------- */

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
	esName := fmt.Sprintf("%s-%s", svcName, instance)
	addrType := discoveryv1.AddressTypeIPv4
	ready := true
	labels := map[string]string{
		"kubernetes.io/service-name": svcName, // associates slice to Service
		kdvLabelOwned:                "true",
		kdvLabelInst:                 instance,
	}
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            esName,
			Namespace:       ns,
			Labels:          labels,
			OwnerReferences: ownerRefToLease(lease),
		},
		AddressType: addrType,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(ready)}},
		},
		Ports: []discoveryv1.EndpointPort{
			{Name: ptr("http"), Protocol: ptr(corev1.ProtocolTCP), Port: ptr(port)},
		},
	}
	if _, err := cs.DiscoveryV1().EndpointSlices(ns).Get(ctx, esName, metav1.GetOptions{}); err == nil {
		_, err := cs.DiscoveryV1().EndpointSlices(ns).Update(ctx, es, metav1.UpdateOptions{})
		return err
	}
	_, err := cs.DiscoveryV1().EndpointSlices(ns).Create(ctx, es, metav1.CreateOptions{})
	return err
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
			log.Printf("[linkerdev] stale lease %s/%s — sweeping", ns, leaseName)
			_ = lg.Delete(ctx, leaseName, metav1.DeleteOptions{})

			sel := metav1.ListOptions{LabelSelector: kdvLabelOwned + "=true"}
			_ = cs.AppsV1().Deployments(ns).Delete(ctx, relayName, metav1.DeleteOptions{})
			_ = cs.CoreV1().Services(ns).Delete(ctx, relayName, metav1.DeleteOptions{})

			if svcs, err := cs.CoreV1().Services(ns).List(ctx, sel); err == nil {
				for _, s := range svcs.Items {
					_ = cs.CoreV1().Services(ns).Delete(ctx, s.Name, metav1.DeleteOptions{})
				}
			}
			if es, err := cs.DiscoveryV1().EndpointSlices(ns).List(ctx, sel); err == nil {
				for _, e := range es.Items {
					_ = cs.DiscoveryV1().EndpointSlices(ns).Delete(ctx, e.Name, metav1.DeleteOptions{})
				}
			}
			_ = deleteHTTPRoute(ctx, dyn, ns, svc+"-route")
			_ = deleteGRPCRoute(ctx, dyn, ns, svc+"-grpc-route")
		} else {
			return fmt.Errorf("another linkerdev instance appears active (lease fresh)")
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

// mustLease is like must() but returns the lease value on success.
func mustLease(l *coordv1.Lease, err error) *coordv1.Lease {
	if err != nil {
		log.Fatal(err)
	}
	return l
}

/* ---------- Linux child with private resolv.conf (safe no-op today) ---------- */

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

/* ---------- misc ---------- */

func remotePortFor(svc, ns string) int {
	h := sha1.Sum([]byte(ns + "/" + svc))
	n := binary.BigEndian.Uint16(h[:2])
	return 20000 + int(n%10000) // 20000..29999
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

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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

/* ---------- DNS Server ---------- */

func recordsForAllServices(ctx context.Context, cs *kubernetes.Clientset) map[string]string {
	records := make(map[string]string)

	// Get all services from all namespaces
	services, err := cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Warning: failed to list services for DNS: %v", err)
		return records
	}

	for _, svc := range services.Items {
		// Create DNS record for service.namespace.svc.cluster.local
		fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
		records[fqdn] = svc.Spec.ClusterIP

		// Also create record for service.namespace
		shortName := fmt.Sprintf("%s.%s", svc.Name, svc.Namespace)
		records[shortName] = svc.Spec.ClusterIP
	}

	return records
}

func startOptionalDNS(records map[string]string) error {
	if runtime.GOOS != "darwin" {
		return nil // DNS server only needed on macOS
	}

	if len(records) == 0 {
		return nil // No records to serve
	}

	// Start DNS server on port 1053
	addr := "127.0.0.1:1053"
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Printf("Warning: failed to start DNS server: %v", err)
		return err
	}

	log.Printf("DNS server started on %s with %d records", addr, len(records))

	go func() {
		defer pc.Close()
		buffer := make([]byte, 512)

		for {
			n, clientAddr, err := pc.ReadFrom(buffer)
			if err != nil {
				// Check for fatal errors that should stop the server
				if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
					log.Printf("DNS temporary error: %v", err)
					continue
				}
				log.Printf("DNS fatal error, stopping server: %v", err)
				return
			}

			// Simple DNS response (just return the IP for A records)
			// This is a minimal implementation - in production you'd want a proper DNS library
			response := createSimpleDNSResponse(buffer[:n], records)
			if len(response) > 0 {
				if _, err := pc.WriteTo(response, clientAddr); err != nil {
					log.Printf("DNS write error: %v", err)
				}
			}
		}
	}()

	return nil
}

func createSimpleDNSResponse(query []byte, records map[string]string) []byte {
	// This is a very basic DNS response implementation
	// For production use, you'd want to use a proper DNS library like github.com/miekg/dns

	if len(query) < 12 {
		return nil
	}

	// Extract the question name from the DNS query
	// This is a simplified parser - real DNS parsing is more complex
	questionStart := 12
	var name string
	var i int
	for i = questionStart; i < len(query) && query[i] != 0; i++ {
		length := int(query[i])
		if i+length >= len(query) {
			return nil
		}
		if len(name) > 0 {
			name += "."
		}
		name += string(query[i+1 : i+1+length])
		i += length
	}

	// Look up the name in our records
	ip, exists := records[name]
	if !exists {
		return nil // No record found
	}

	// Parse IP address
	ipAddr := net.ParseIP(ip)
	if ipAddr == nil {
		return nil
	}

	// Create a simple DNS response
	// This is a minimal implementation - proper DNS responses are more complex
	response := make([]byte, len(query))
	copy(response, query)

	// Set response flags (QR=1, AA=1, RA=1)
	response[2] = 0x81
	response[3] = 0x80

	// Set answer count to 1
	response[6] = 0x00
	response[7] = 0x01

	// Add the answer record
	response = append(response, query[questionStart:i+5]...) // Copy question
	response = append(response, 0x00, 0x01)                  // Type A
	response = append(response, 0x00, 0x01)                  // Class IN
	response = append(response, 0x00, 0x00, 0x00, 0x3c)      // TTL 60 seconds
	response = append(response, 0x00, 0x04)                  // Data length 4 bytes
	response = append(response, ipAddr.To4()...)             // IP address

	return response
}
