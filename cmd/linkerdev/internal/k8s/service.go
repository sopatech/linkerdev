package k8s

import (
	"context"
	"log"

	coordv1 "k8s.io/api/coordination/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// EnsureDevService creates or updates a dev service
func EnsureDevService(ctx context.Context, cs *kubernetes.Clientset, ns, name string, svcPort, remotePort int32, lease *coordv1.Lease, instance string) error {
	devSvc := name + "-dev"

	_, err := cs.CoreV1().Services(ns).Patch(ctx, devSvc, types.ApplyPatchType,
		[]byte(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"`+devSvc+`","namespace":"`+ns+`","labels":{"app.kdvwrap/owned":"true","app.kdvwrap/instance":"`+instance+`"},"ownerReferences":[{"apiVersion":"coordination.k8s.io/v1","kind":"Lease","name":"`+lease.Name+`","uid":"`+string(lease.UID)+`","controller":true,"blockOwnerDeletion":true}]},"spec":{"type":"ClusterIP","ports":[{"name":"http","port":`+string(rune(svcPort))+`,"targetPort":`+string(rune(remotePort))+`}],"selector":{"app.kdvwrap/owned":"true","app.kdvwrap/instance":"`+instance+`"}}}`),
		metav1.PatchOptions{FieldManager: "linkerdev"})

	if err != nil {
		log.Printf("Failed to create dev service: %v", err)
		return err
	}

	log.Printf("Created dev service: %s", devSvc)
	return nil
}

// EnsureDevEndpointSlice creates or updates an endpoint slice for the dev service
func EnsureDevEndpointSlice(ctx context.Context, cs *kubernetes.Clientset, ns, svcName, ip string, port int32, lease *coordv1.Lease, instance string) error {
	devSvc := svcName + "-dev"
	eps := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      devSvc + "-eps",
			Namespace: ns,
			Labels: map[string]string{
				"app.kdvwrap/owned": "true", "app.kdvwrap/instance": instance,
				"kubernetes.io/service-name": devSvc,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "coordination.k8s.io/v1", Kind: "Lease",
				Name: lease.Name, UID: lease.UID,
				Controller: func() *bool { v := true; return &v }(), BlockOwnerDeletion: func() *bool { v := true; return &v }(),
			}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: func() *bool { v := true; return &v }()},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name: func() *string { v := "http"; return &v }(), Port: func() *int32 { return &port }(),
		}},
	}

	_, err := cs.DiscoveryV1().EndpointSlices(ns).Patch(ctx, eps.Name, types.ApplyPatchType,
		[]byte(`{"apiVersion":"discovery.k8s.io/v1","kind":"EndpointSlice","metadata":{"name":"`+eps.Name+`","namespace":"`+ns+`","labels":{"app.kdvwrap/owned":"true","app.kdvwrap/instance":"`+instance+`","kubernetes.io/service-name":"`+devSvc+`"},"ownerReferences":[{"apiVersion":"coordination.k8s.io/v1","kind":"Lease","name":"`+lease.Name+`","uid":"`+string(lease.UID)+`","controller":true,"blockOwnerDeletion":true}]},"addressType":"IPv4","endpoints":[{"addresses":["`+ip+`"],"conditions":{"ready":true}}],"ports":[{"name":"http","port":`+string(rune(port))+`}]}`),
		metav1.PatchOptions{FieldManager: "linkerdev"})

	if err != nil {
		log.Printf("Failed to create endpoint slice: %v", err)
		return err
	}

	log.Printf("Created endpoint slice: %s", eps.Name)
	return nil
}
