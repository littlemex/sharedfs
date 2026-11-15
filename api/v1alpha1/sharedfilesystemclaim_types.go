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
)

// FilesystemReference names a SharedFilesystem.
type FilesystemReference struct {
	// name of the SharedFilesystem.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SharedFilesystemClaimSpec asks for a mount of a SharedFilesystem in this namespace.
// The whole spec is immutable: to change it, delete the claim and create a new one.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; delete and recreate the claim"
type SharedFilesystemClaimSpec struct {
	// filesystemRef names the SharedFilesystem to mount.
	// +required
	FilesystemRef FilesystemReference `json:"filesystemRef"`

	// access is the kind of mount to create.
	// +optional
	// +kubebuilder:default=ReadOnly
	Access Access `json:"access,omitempty"`
}

// SharedFilesystemClaimStatus is the observed state of a SharedFilesystemClaim.
type SharedFilesystemClaimStatus struct {
	// observedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// persistentVolumeName is the generated PersistentVolume.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`

	// conditions: Accepted, Bound, Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sfc,categories=sharedfs
// +kubebuilder:printcolumn:name="Filesystem",type=string,JSONPath=`.spec.filesystemRef.name`
// +kubebuilder:printcolumn:name="Access",type=string,JSONPath=`.spec.access`
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=`.status.persistentVolumeName`,priority=1
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SharedFilesystemClaim asks for the SharedFilesystem it names to be mountable in this
// namespace. Pods mount the PersistentVolumeClaim that the controller creates with the claim's
// own name. It is created by the tenant.
type SharedFilesystemClaim struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec SharedFilesystemClaimSpec `json:"spec"`

	// +optional
	Status SharedFilesystemClaimStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SharedFilesystemClaimList contains a list of SharedFilesystemClaim.
type SharedFilesystemClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SharedFilesystemClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SharedFilesystemClaim{}, &SharedFilesystemClaimList{})
		return nil
	})
}
