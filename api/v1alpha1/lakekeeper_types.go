/*
Copyright 2024.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type LakekeeperOauth2 struct {
	// The client id of the OIDC App of the catalog. The aud (audience) claim of the JWT token must match this id.
	ClientId string `json:"client_id"`

	// If set, access to rest endpoints is secured via an external OIDC-capable IdP. The IdP must expose {provider_url}/.well-known/openid-configuration and the openid-configuration needs to have the jwks_uri and issuer defined. For keycloak set: https://keycloak.local/realms/test For Entra-ID set: https://login.microsoftonline.com/{your-tenant-id}/v2.0
	ProviderUri string `json:"provider_uri"`
}

type LakekeeperAuth struct {
	Oauth2 LakekeeperOauth2 `json:"oauth2,omitempty"`
}

type LakekeeperImage struct {
	// 65534 = nobody of google container distroless
	//+kubebuilder:default=65534
	Gid int32 `json:"gid,omitempty"`
	// The image pull policy
	//+kubebuilder:default="IfNotPresent"
	PullPolicy string `json:"pullPolicy"`
	// The image repository to pull from
	//+kubebuilder:default="quay.io/lakekeeper/catalog"
	Repository string `json:"repository"`
	// The image tag to pull
	//+kubebuilder:default="v0.4.3"
	Tag string `json:"tag"`
	// 65532 = nonroot of google container distroless
	//+kubebuilder:default=65532
	Uid int32 `json:"uid"`
}

type LakekeeperCatalog struct {
	Image LakekeeperImage `json:"image"`
	// Number of replicas to deploy. Replicas are stateless.
	//+kubebuilder:default=1
	Replicas int32 `json:"replicas"`
}

// LakekeeperSpec defines the desired state of Lakekeeper
type LakekeeperSpec struct {
	Auth    LakekeeperAuth    `json:"auth,omitempty"`
	Catalog LakekeeperCatalog `json:"catalog,omitempty"`
}

// LakekeeperStatus defines the observed state of Lakekeeper
type LakekeeperStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Lakekeeper is the Schema for the lakekeepers API
type Lakekeeper struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LakekeeperSpec   `json:"spec,omitempty"`
	Status LakekeeperStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// LakekeeperList contains a list of Lakekeeper
type LakekeeperList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Lakekeeper `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Lakekeeper{}, &LakekeeperList{})
}
