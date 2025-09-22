package k8s

import (
	"context"
	"log"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// EnsureLease creates or updates a lease for instance tracking
func EnsureLease(ctx context.Context, cs *kubernetes.Clientset, ns, name, holder string) (*coordv1.Lease, error) {
	result, err := cs.CoordinationV1().Leases(ns).Patch(ctx, name, types.ApplyPatchType,
		[]byte(`{"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":"`+name+`","namespace":"`+ns+`","labels":{"app.kdvwrap/owned":"true","app.kdvwrap/instance":"`+holder+`"}},"spec":{"holderIdentity":"`+holder+`","leaseDurationSeconds":30,"renewTime":"`+time.Now().Format(time.RFC3339)+`"}}`),
		metav1.PatchOptions{FieldManager: "linkerdev"})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// RenewLeaseLoop continuously renews a lease
func RenewLeaseLoop(cs *kubernetes.Clientset, ns, name, holder string, stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := cs.CoordinationV1().Leases(ns).Update(ctx, &coordv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: coordv1.LeaseSpec{
					HolderIdentity:       &holder,
					LeaseDurationSeconds: func() *int32 { v := int32(30); return &v }(),
					RenewTime:            &metav1.MicroTime{Time: time.Now()},
				},
			}, metav1.UpdateOptions{})
			cancel()
			if err != nil {
				log.Printf("Failed to renew lease: %v", err)
			}
		case <-stop:
			return
		}
	}
}

// IsLeaseStale checks if a lease is stale
func IsLeaseStale(l *coordv1.Lease) bool {
	if l.Spec.RenewTime == nil {
		return true
	}
	return time.Since(l.Spec.RenewTime.Time) > 60*time.Second
}

// MustLease is like must() but returns the lease value on success
func MustLease(l *coordv1.Lease, err error) *coordv1.Lease {
	if err != nil {
		log.Fatal(err)
	}
	return l
}
