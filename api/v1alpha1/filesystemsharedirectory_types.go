/*
Copyright 2026.

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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// ShareReference identifies a FilesystemShare by name and UID.
type ShareReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// FilesystemShareDirectorySpec records which namespace a per-namespace directory belongs to. The
// controller writes it when the namespace first claims the share; the whole spec is immutable.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type FilesystemShareDirectorySpec struct {
	ShareRef ShareReference `json:"shareRef"`
	// namespace whose claims mount this directory.
	Namespace string `json:"namespace"`
	// namespaceUID of that namespace when the directory was first claimed. A namespace of the
	// same name with another UID is a different tenant: its claims are not provisioned until an
	// admin deletes this object.
	NamespaceUID types.UID `json:"namespaceUID"`
	// path is the expanded path inside the file system.
	Path string `json:"path"`
}

// ObservedDirectory is what the preparer found or created.
type ObservedDirectory struct {
	UID  int64  `json:"uid"`
	GID  int64  `json:"gid"`
	Mode string `json:"mode"`
}

// FilesystemShareDirectoryStatus is the observed state of the directory.
type FilesystemShareDirectoryStatus struct {
	// +optional
	Observed *ObservedDirectory `json:"observed,omitempty"`
	// failures counts preparer runs that did not prepare the directory; retries back off
	// from 30s, doubling, to one hour.
	// +optional
	Failures int32 `json:"failures,omitempty"`
	// lastAttemptTime is when the latest preparer pod was started.
	// +optional
	LastAttemptTime *metav1.Time `json:"lastAttemptTime,omitempty"`
	// conditions: Prepared.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fsd,categories=sharedfs
// +kubebuilder:printcolumn:name="Share",type=string,JSONPath=`.spec.shareRef.name`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="Prepared",type=string,JSONPath=`.status.conditions[?(@.type=="Prepared")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Prepared")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FilesystemShareDirectory binds a per-namespace directory of a FilesystemShare to the namespace
// (by UID) that first claimed it, and reports its preparation. The controller creates it;
// deleting it is how an admin lets a recreated namespace take the directory over.
type FilesystemShareDirectory struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec FilesystemShareDirectorySpec `json:"spec"`
	// +optional
	Status FilesystemShareDirectoryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FilesystemShareDirectoryList contains a list of FilesystemShareDirectory.
type FilesystemShareDirectoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FilesystemShareDirectory `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FilesystemShareDirectory{}, &FilesystemShareDirectoryList{})
		return nil
	})
}
