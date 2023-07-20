// Package controllers implements functions for manipulating CAPI generated
// cluster secrets into Argo definitions.
package controllers

import (
	b64 "encoding/base64"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	// ArgoNamespace represents the Namespace that hold ArgoCluster secrets.
	ArgoNamespace string
	ArgoEndpoint string
	ArgoRemote bool
	authToken string
)

// GetArgoCommonLabels holds a map of labels that reconciled objects must have.
func GetArgoCommonLabels() map[string]string {
	return map[string]string{
		"capi-to-argocd/owned":           "true",
		"argocd.argoproj.io/secret-type": "cluster",
	}
}

// ArgoCluster holds all information needed for CAPI --> Argo Cluster conversion

type ClusterList struct {
	Clusters		[]ArgoCluster `json:"items"`
}

type ArgoCluster struct {
	NamespacedName types.NamespacedName
	ClusterName    string `json:"name"`
	ClusterServer  string `json:"server"`
	ClusterLabels  map[string]string `json:"labels"`
	ClusterConfig  ArgoConfig `json:"config"`
}


// ArgoConfig represents Argo Cluster.JSON.config
type ArgoConfig struct {
	TLSClientConfig ArgoTLS `json:"tlsClientConfig"`
}

// ArgoTLS represents Argo Cluster.JSON.config.tlsClientConfig
type ArgoTLS struct {
	CaData   string `json:"caData"`
	CertData string `json:"certData"`
	KeyData  string `json:"keyData"`
}

// NewArgoCluster return a new ArgoCluster
func NewArgoCluster(c *CapiCluster, s *corev1.Secret) *ArgoCluster {
	return &ArgoCluster{
		NamespacedName: BuildNamespacedName(s.ObjectMeta.Name, s.ObjectMeta.Namespace),
		ClusterName:    BuildClusterName(c.KubeConfig.Clusters[0].Name, s.ObjectMeta.Namespace),
		ClusterServer:  c.KubeConfig.Clusters[0].Cluster.Server,
		ClusterLabels: map[string]string{
			"capi-to-argocd/cluster-secret-name": c.Name + "-kubeconfig",
			"capi-to-argocd/cluster-namespace":   c.Namespace,
		},
		ClusterConfig: ArgoConfig{
			TLSClientConfig: ArgoTLS{
				CaData:   c.KubeConfig.Clusters[0].Cluster.CaData,
				CertData: c.KubeConfig.Users[0].User.CertData,
				KeyData:  c.KubeConfig.Users[0].User.KeyData,
			},
		},
	}
}

// BuildNamespacedName returns k8s native object identifier.
func BuildNamespacedName(s string, namespace string) types.NamespacedName {
	return types.NamespacedName{
		Name:      "cluster-" + BuildClusterName(strings.TrimSuffix(s, "-kubeconfig"), namespace),
		Namespace: ArgoNamespace,
	}
}

// BuildClusterName returns cluster name after transformations applied (with/without namespace suffix, etc).
func BuildClusterName(s string, namespace string) string {
	prefix := ""
	if EnableNamespacedNames {
		prefix += namespace + "-"
	}
	return prefix + s
}

// ValidateClusterTLSConfig validates that we got proper based64 k/v fields.
func ValidateClusterTLSConfig(a *ArgoTLS) error {
	for _, v := range []string{a.CaData, a.CertData, a.KeyData} {
		// Check if field.value is empty
		if v == "" {
			return errors.New("missing key on ArgoTLS config")
		}
		// Check if field.value is valid b64 encoded string
		if _, err := b64.StdEncoding.DecodeString(v); err != nil {
			return err
		}
	}
	return nil
}
