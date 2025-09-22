package k8s

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// EnsureRelay creates or updates the relay deployment
func EnsureRelay(ctx context.Context, cs *kubernetes.Clientset, ns string, remoteRPort int32, lease *coordv1.Lease, instance string) (string, error) {
	name := "linkerdev-relay"

	_, err := cs.AppsV1().Deployments(ns).Patch(ctx, name, types.ApplyPatchType,
		[]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"`+name+`","namespace":"`+ns+`","labels":{"app.kdvwrap/owned":"true","app.kdvwrap/instance":"`+instance+`"},"ownerReferences":[{"apiVersion":"coordination.k8s.io/v1","kind":"Lease","name":"`+lease.Name+`","uid":"`+string(lease.UID)+`","controller":true,"blockOwnerDeletion":true}]},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"`+name+`"}},"template":{"metadata":{"labels":{"app":"`+name+`","app.kdvwrap/owned":"true","app.kdvwrap/instance":"`+instance+`"}},"spec":{"containers":[{"name":"relay","image":"linkerdev-relay:latest","ports":[{"name":"relay","containerPort":`+string(rune(remoteRPort))+`,"protocol":"TCP"}]}]}}}}`),
		metav1.PatchOptions{FieldManager: "linkerdev"})

	if err != nil {
		return "", err
	}

	// Wait for pod to be ready
	ip, err := WaitForPodIP(ctx, cs, ns, "app="+name, 30*time.Second)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s:%d", ip, remoteRPort), nil
}

// StartKubectlPortForward starts a kubectl port-forward command
func StartKubectlPortForward(ctx context.Context, ns, name string, local, remote int) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", ns, name, fmt.Sprintf("%d:%d", local, remote))
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd, cmd.Start()
}

// WaitForPodIP waits for a pod to be ready and returns its IP
func WaitForPodIP(ctx context.Context, cs *kubernetes.Clientset, ns, selector string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				continue
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
					return pod.Status.PodIP, nil
				}
			}
		}
	}
}
