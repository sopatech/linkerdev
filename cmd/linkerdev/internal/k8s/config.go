package k8s

import (
	"log"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// LoadKubeConfigOrDie loads Kubernetes configuration
func LoadKubeConfigOrDie() *rest.Config {
	var cfg *rest.Config
	var err error
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
	}
	if err != nil {
		log.Fatalf("Failed to load kubeconfig: %v", err)
	}
	return cfg
}

// NewClientset creates a new Kubernetes clientset
func NewClientset() *kubernetes.Clientset {
	cfg := LoadKubeConfigOrDie()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}
	return cs
}
