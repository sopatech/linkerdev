package utils

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// must panics if err is not nil
func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// mustLease is like must() but returns the lease value on success
func mustLease(l *coordv1.Lease, err error) *coordv1.Lease {
	if err != nil {
		log.Fatal(err)
	}
	return l
}

// ptr returns a pointer to the given value
func ptr[T any](v T) *T { return &v }

// shQuote quotes a string for shell usage
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isValidKubernetesName validates a Kubernetes resource name
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

// remotePortFor generates a deterministic port for a service
func remotePortFor(svc, ns string) int {
	h := sha1.Sum([]byte(ns + "/" + svc))
	n := binary.BigEndian.Uint16(h[:2])
	return 20000 + int(n%10000) // 20000..29999
}

// randomFreePort finds a random free port
func randomFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	_, pStr, _ := net.SplitHostPort(l.Addr().String())
	return strconv.Atoi(pStr)
}

// instanceID generates a unique instance ID
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

// ownerRefToLease creates owner references for a lease
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
