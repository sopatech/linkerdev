package commands

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sopatech/linkerdev/cmd/linkerdev/internal/k8s"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// InstallRelay installs the relay component in the cluster
func InstallRelay(version string) {
	log.Println("Installing linkerdev relay component...")

	// Create Kubernetes client
	cs := k8s.NewClientset()
	ctx := context.Background()

	// Install in kube-system namespace
	ns := "kube-system"
	relayName := "linkerdev-relay"

	// Create relay deployment
	_ = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relayName,
			Namespace: ns,
			Labels: map[string]string{
				"app": relayName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: func() *int32 { v := int32(1); return &v }(),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": relayName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": relayName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "relay",
							Image: "ghcr.io/sopatech/linkerdev-relay:" + version,
							Args: []string{
								"--listen", "20000",
								"--control", "18080",
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "ctrl",
									ContainerPort: 18080,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "rev",
									ContainerPort: 20000,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(18080),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       2,
								FailureThreshold:    3,
							},
						},
					},
				},
			},
		},
	}

	// Apply deployment
	deploymentJSON := fmt.Sprintf(`{
		"apiVersion": "apps/v1",
		"kind": "Deployment",
		"metadata": {
			"name": "%s",
			"namespace": "%s",
			"labels": {
				"app": "%s"
			}
		},
		"spec": {
			"replicas": 1,
			"selector": {
				"matchLabels": {
					"app": "%s"
				}
			},
			"template": {
				"metadata": {
					"labels": {
						"app": "%s"
					}
				},
				"spec": {
					"containers": [{
						"name": "relay",
						"image": "ghcr.io/sopatech/linkerdev-relay:%s",
						"args": ["--listen", "20000", "--control", "18080"],
						"ports": [
							{"name": "ctrl", "containerPort": 18080, "protocol": "TCP"},
							{"name": "rev", "containerPort": 20000, "protocol": "TCP"}
						],
						"readinessProbe": {
							"httpGet": {
								"path": "/healthz",
								"port": 18080
							},
							"initialDelaySeconds": 1,
							"periodSeconds": 2,
							"failureThreshold": 3
						}
					}]
				}
			}
		}
	}`, relayName, ns, relayName, relayName, relayName, version)

	_, err := cs.AppsV1().Deployments(ns).Patch(ctx, relayName, types.ApplyPatchType,
		[]byte(deploymentJSON),
		metav1.PatchOptions{FieldManager: "linkerdev"})

	if err != nil {
		log.Fatalf("Failed to create relay deployment: %v", err)
	}

	// Create relay service
	_ = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relayName,
			Namespace: ns,
			Labels: map[string]string{
				"app": relayName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "ctrl",
					Port:       18080,
					TargetPort: intstr.FromInt(18080),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "rev",
					Port:       20000,
					TargetPort: intstr.FromInt(20000),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: map[string]string{
				"app": relayName,
			},
		},
	}

	serviceJSON := fmt.Sprintf(`{
		"apiVersion": "v1",
		"kind": "Service",
		"metadata": {
			"name": "%s",
			"namespace": "%s",
			"labels": {
				"app": "%s"
			}
		},
		"spec": {
			"type": "ClusterIP",
			"ports": [
				{"name": "ctrl", "port": 18080, "targetPort": 18080, "protocol": "TCP"},
				{"name": "rev", "port": 20000, "targetPort": 20000, "protocol": "TCP"}
			],
			"selector": {
				"app": "%s"
			}
		}
	}`, relayName, ns, relayName, relayName)

	_, err = cs.CoreV1().Services(ns).Patch(ctx, relayName, types.ApplyPatchType,
		[]byte(serviceJSON),
		metav1.PatchOptions{FieldManager: "linkerdev"})

	if err != nil {
		log.Fatalf("Failed to create relay service: %v", err)
	}

	// Wait for deployment to be ready
	log.Println("Waiting for relay deployment to be ready...")
	timeout := 60 * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Fatalf("Timeout waiting for relay deployment to be ready")
		case <-ticker.C:
			deployment, err := cs.AppsV1().Deployments(ns).Get(ctx, relayName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if deployment.Status.ReadyReplicas >= 1 {
				log.Println("Relay component installed successfully!")
				return
			}
		}
	}
}

// UninstallRelay removes the relay component from the cluster
func UninstallRelay() {
	log.Println("Uninstalling linkerdev relay component...")

	// Create Kubernetes client
	cs := k8s.NewClientset()
	ctx := context.Background()

	// Remove from kube-system namespace
	ns := "kube-system"
	relayName := "linkerdev-relay"

	// Delete deployment
	err := cs.AppsV1().Deployments(ns).Delete(ctx, relayName, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("Failed to delete relay deployment: %v", err)
	} else {
		log.Println("Deleted relay deployment")
	}

	// Delete service
	err = cs.CoreV1().Services(ns).Delete(ctx, relayName, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("Failed to delete relay service: %v", err)
	} else {
		log.Println("Deleted relay service")
	}

	log.Println("Relay component uninstalled successfully!")
}

// CleanupResources cleans up leftover resources
func CleanupResources(ctx context.Context, cs *kubernetes.Clientset, dyn dynamic.Interface, ns, svc string) {
	log.Printf("Cleaning up resources for service %s in namespace %s", svc, ns)

	// Clean up TrafficSplit resources
	if svc != "" {
		trafficSplitName := svc + "-split"
		err := dyn.Resource(linkerdTrafficSplitGVR).Namespace(ns).Delete(ctx, trafficSplitName, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("Failed to delete TrafficSplit %s: %v", trafficSplitName, err)
		} else {
			log.Printf("Deleted TrafficSplit: %s", trafficSplitName)
		}

		// Clean up dev service
		devSvcName := svc + "-dev"
		err = cs.CoreV1().Services(ns).Delete(ctx, devSvcName, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("Failed to delete dev service %s: %v", devSvcName, err)
		} else {
			log.Printf("Deleted dev service: %s", devSvcName)
		}

		// Clean up endpoint slice
		epsName := devSvcName + "-eps"
		err = cs.DiscoveryV1().EndpointSlices(ns).Delete(ctx, epsName, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("Failed to delete endpoint slice %s: %v", epsName, err)
		} else {
			log.Printf("Deleted endpoint slice: %s", epsName)
		}
	}

	// Clean up leases with linkerdev labels
	leases, err := cs.CoordinationV1().Leases(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kdvwrap/owned=true",
	})
	if err != nil {
		log.Printf("Failed to list leases: %v", err)
	} else {
		for _, lease := range leases.Items {
			err = cs.CoordinationV1().Leases(ns).Delete(ctx, lease.Name, metav1.DeleteOptions{})
			if err != nil {
				log.Printf("Failed to delete lease %s: %v", lease.Name, err)
			} else {
				log.Printf("Deleted lease: %s", lease.Name)
			}
		}
	}

	log.Println("Cleanup completed!")
}

// linkerdTrafficSplitGVR is the GroupVersionResource for Linkerd TrafficSplit
var linkerdTrafficSplitGVR = schema.GroupVersionResource{
	Group:    "split.smi-spec.io",
	Version:  "v1alpha1",
	Resource: "trafficsplits",
}
