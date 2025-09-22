package linkerd

import (
	"context"
	"encoding/json"
	"log"

	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	kdvLabelOwned = "app.kdvwrap/owned"
	kdvLabelInst  = "app.kdvwrap/instance"
)

// ApplyTrafficSplit creates a TrafficSplit to divert 100% traffic from original service to dev service
func ApplyTrafficSplit(ctx context.Context, dyn dynamic.Interface, ns, svc, devSvc string, lease *coordv1.Lease, instance string) error {
	// TrafficSplit uses SMI specification
	gvr := schema.GroupVersionResource{
		Group:    "split.smi-spec.io",
		Version:  "v1alpha2",
		Resource: "trafficsplits",
	}

	name := svc + "-split"
	obj := map[string]any{
		"apiVersion": "split.smi-spec.io/v1alpha2",
		"kind":       "TrafficSplit",
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
			"service": svc, // Original service name
			"backends": []map[string]any{
				{
					"service": devSvc, // Dev service gets 100% traffic
					"weight":  100,
				},
				{
					"service": svc, // Original service gets 0% traffic
					"weight":  0,
				},
			},
		},
	}

	b, _ := json.Marshal(obj)
	log.Printf("TrafficSplit: Creating traffic split %s to route 100%% traffic from %s to %s", name, svc, devSvc)

	if result, err := dyn.Resource(gvr).Namespace(ns).Patch(ctx, name, types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: "linkerdev"}); err == nil {
		log.Printf("TrafficSplit: Successfully created traffic split %s, result: %v", name, result)
		return nil
	} else {
		log.Printf("TrafficSplit: Failed to create traffic split %s: %v", name, err)
		return err
	}
}

// DeleteTrafficSplit deletes a TrafficSplit resource
func DeleteTrafficSplit(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	gvr := schema.GroupVersionResource{
		Group:    "split.smi-spec.io",
		Version:  "v1alpha2",
		Resource: "trafficsplits",
	}

	log.Printf("TrafficSplit: Deleting traffic split %s", name)
	return dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
}
