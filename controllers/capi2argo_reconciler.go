package controllers

import (
	"bytes"
	"context"
	//goErr "errors"
	"os"
	"strconv"
	"net/http"
	"net/url"
	
	"fmt"
	"encoding/json"
	"io/ioutil"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	//"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

var (
	// EnableGarbageCollection enables experimental GC feature
	EnableGarbageCollection bool

	// EnableNamespacedNames represents a mode where the cluster name is always
	// prepended by the cluster namespace in all generated secrets
	EnableNamespacedNames bool
)

func init() {
	// Dummy configuration init.
	// TODO: Handle this as part of root config.
	authToken = os.Getenv("ARGOCD_AUTHTOKEN")
	ArgoEndpoint = os.Getenv("ARGOCD_ENDPOINT")
	if ArgoEndpoint == "" {
		ArgoEndpoint = "argocd-server.argocd.svc.cluster.local"
	}

	EnableGarbageCollection, _ = strconv.ParseBool(os.Getenv("ENABLE_GARBAGE_COLLECTION"))
	EnableNamespacedNames, _ = strconv.ParseBool(os.Getenv("ENABLE_NAMESPACED_NAMES"))
}

// Capi2Argo reconciles a Secret object
type Capi2Argo struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
	HttpClient *http.Client

}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets/status,verbs=get;update;patch

// Reconcile holds all the logic for syncing CAPI to Argo Clusters.
func (r *Capi2Argo) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("secret", req.NamespacedName)

	// TODO: Check if secret is on allowed Namespaces.

	// Validate Secret.Metadata.Name complies with CAPI pattern: <clusterName>-kubeconfig
	if !ValidateCapiNaming(req.NamespacedName) {
		return ctrl.Result{}, nil
	}

	// Fetch CapiSecret
	var capiSecret corev1.Secret
	err := r.Get(ctx, req.NamespacedName, &capiSecret)
	if err != nil {
		// If we get error reading the object - requeue the request.
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}

		// If secret is deleted and GC is enabled, mark ArgoSecret for deletion.
		if EnableGarbageCollection {

			apiurl := fmt.Sprintf("https://%s/api/v1/clusters/%s?id.type=name",ArgoEndpoint, req.NamespacedName.Namespace)

			_, err:= sendRequest (r.HttpClient, "DELETE", apiurl, authToken, nil)
			if err != nil {
				log.Error(err, "Error on reconcile/delete")
				return ctrl.Result{}, err
			}

			log.Info("Deleted successfully of ArgoSecret")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Fetched CapiSecret")

	// Validate CapiSecret.type is matching CAPI convention.
	// if capiSecret.Type != "cluster.x-k8s.io/secret" {
	err = ValidateCapiSecret(&capiSecret)
	if err != nil {
		log.Info("Ignoring secret as it's missing proper CAPI type", "type", capiSecret.Type)
		return ctrl.Result{}, err
	}

	// Construct CapiCluster from CapiSecret.
	nn := strings.TrimSuffix(req.NamespacedName.Name, "-kubeconfig")
	ns := req.NamespacedName.Namespace
	capiCluster := NewCapiCluster(nn, ns)
	err = capiCluster.Unmarshal(&capiSecret)
	if err != nil {
		log.Error(err, "Failed to unmarshal CapiCluster")
		return ctrl.Result{}, err
	}

	argoCluster := NewArgoCluster(capiCluster, &capiSecret)

	for key, value := range GetArgoCommonLabels() {
		argoCluster.ClusterLabels[key] = value
	}
	
	// Check if ArgoCluster already exists via API.
	apiurl := fmt.Sprintf("https://%s/api/v1/clusters",ArgoEndpoint)
	bodyBytes, err:= sendRequest (r.HttpClient, "GET", apiurl, authToken, nil)

	exists:= false
	var clusterList ClusterList
	var existingCluster ArgoCluster

	if err = json.Unmarshal(bodyBytes, &clusterList); err != nil {
		log.Error(err, "Error decoding JSON response")
		return ctrl.Result{}, err
	}
	// Iterate over the payloads
	for _, cluster := range clusterList.Clusters {
		if cluster.ClusterServer == argoCluster.ClusterServer {
			if cluster.ClusterLabels["capi-to-argocd/owned"] == "true" {
				exists = true
				existingCluster = cluster
			}
		}
	}
	
	// Reconcile ArgoCluster:
	// - If does not exists:
	//     1) Create it.
	// - If exists:
	//     1) Parse labels and check if it is meant to be managed by the controller.
	//     2) If it is controller-managed, check if updates needed and apply them.
	switch exists {
	case false:
		// Create Cluster via API

		apiurl := fmt.Sprintf("https://%s/api/v1/clusters",ArgoEndpoint)

		jsonData, err := json.Marshal(argoCluster)
		if err != nil {
			log.Error(err, "Error on marshalling")
			return ctrl.Result{}, err
		}
		apiurl = fmt.Sprintf("https://%s/api/v1/clusters",ArgoEndpoint)
		_, err = sendRequest (r.HttpClient, "POST", apiurl, authToken, jsonData)
		if err != nil {
			log.Error(err, "Error on creating Cluster")
			return ctrl.Result{}, err
		}

		log.Info("Created new ArgoSecret")
		return ctrl.Result{}, nil
	case true:
		log.Info("Checking if ArgoSecret is out-of-sync with")
		changed := false
		if existingCluster.ClusterName != argoCluster.ClusterName {
			existingCluster.ClusterName = argoCluster.ClusterName
			changed = true
		}
		if existingCluster.ClusterConfig.TLSClientConfig.CaData != argoCluster.ClusterConfig.TLSClientConfig.CaData {
			existingCluster.ClusterConfig.TLSClientConfig.CaData = argoCluster.ClusterConfig.TLSClientConfig.CaData
			changed = true
		}
		if existingCluster.ClusterConfig.TLSClientConfig.CertData != argoCluster.ClusterConfig.TLSClientConfig.CertData {
			existingCluster.ClusterConfig.TLSClientConfig.CertData = argoCluster.ClusterConfig.TLSClientConfig.CertData
			changed = true
		}
		if changed {
			log.Info("Updating out-of-sync ArgoSecret")
			apiurl := fmt.Sprintf("https://%s/api/v1/clusters/%s",ArgoEndpoint, url.QueryEscape(existingCluster.ClusterServer))

			jsonData, err := json.Marshal(existingCluster)
			if err != nil {
				log.Error(err, "Error on marshalling")
				return ctrl.Result{}, err
			}

			_, err = sendRequest (r.HttpClient, "PUT", apiurl, authToken, jsonData)
			if err != nil {
				log.Error(err, "Error on Updating")
				return ctrl.Result{}, err
			}

			log.Info("Updated successfully of ArgoSecret")
			return ctrl.Result{}, nil

		}

	}
	
	return ctrl.Result{}, nil
}

// SetupWithManager ..
func (r *Capi2Argo) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&corev1.Secret{}).Complete(r)
}

// sendRequest
func sendRequest(client *http.Client, method, url, authToken string, data []byte) ([]byte, error) {
	var req *http.Request
	var err error

	if data != nil {
		req, err = http.NewRequest(method, url, bytes.NewBuffer(data))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", authToken))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
		
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}