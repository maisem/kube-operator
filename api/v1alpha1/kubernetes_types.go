/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	etcdapi "github.com/coreos/etcd-operator/pkg/apis/etcd/v1beta2"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubernetesSpec defines the desired state of Kubernetes
type KubernetesSpec struct {
	Replicas              int    `json:"replicas"`
	ClusterDomain         string `json:"clusterDomain"`
	ServiceClusterIPRange string `json:"serviceClusterIPRange"`
	Version               string `json:"version"`

	APIGroups []string `json:"apiGroups,omitempty"`

	APIServiceSpec corev1.ServiceSpec  `json:"apiServiceSpec"`
	RootIssuer     IssuerSpec          `json:"rootIssuer"`
	Etcd           etcdapi.ClusterSpec `json:"etcd"`
}

// TODO remove this crap.
// IssuerSpec is a temporary hack around cmapi.IssuerSpec.
type IssuerSpec struct {
	// +optional
	CA *cmapi.CAIssuer `json:"ca,omitempty"`

	// +optional
	Vault *cmapi.VaultIssuer `json:"vault,omitempty"`

	// +optional
	SelfSigned *cmapi.SelfSignedIssuer `json:"selfSigned,omitempty"`

	// +optional
	Venafi *cmapi.VenafiIssuer `json:"venafi,omitempty"`
}

type Endpoint struct {
	Address string `json:"address"`
	Port    int32  `json:"port"`
}

// KubernetesStatus defines the observed state of Kubernetes
type KubernetesStatus struct {
	InternalEndpoint Endpoint `json:"internalEndpoint"`

	// +optional
	ExternalEndpoint *Endpoint `json:"externalEndpoint,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Kubernetes is the Schema for the kubernetes API
type Kubernetes struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubernetesSpec   `json:"spec,omitempty"`
	Status KubernetesStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KubernetesList contains a list of Kubernetes
type KubernetesList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Kubernetes `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Kubernetes{}, &KubernetesList{})
}
