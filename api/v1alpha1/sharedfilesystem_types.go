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

// Access is the kind of mount a tenant gets.
// +kubebuilder:validation:Enum=ReadOnly;ReadWrite
type Access string

const (
	AccessReadOnly  Access = "ReadOnly"
	AccessReadWrite Access = "ReadWrite"
)

// Allows reports whether a mount of kind want fits within the ceiling a.
func (a Access) Allows(want Access) bool {
	return a == AccessReadWrite || want == AccessReadOnly
}

// FilesystemSource names the existing static PersistentVolume that points at the file system.
type FilesystemSource struct {
	// persistentVolumeName is the PersistentVolume whose CSI fields are copied into every
	// generated PersistentVolume. It is only read; its phase (Bound, Released, Available) does
	// not matter.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PersistentVolumeName string `json:"persistentVolumeName"`
}

// AllowedNamespaces selects the namespaces that may claim the file system.
type AllowedNamespaces struct {
	// selector matches namespace labels. An empty selector ({}) matches every namespace.
	// To name namespaces one by one, match on kubernetes.io/metadata.name with the In operator.
	// +required
	Selector metav1.LabelSelector `json:"selector"`
}

// SharedFilesystemSpec defines a file system that the platform admin offers to tenants.
// +kubebuilder:validation:XValidation:rule="self.source == oldSelf.source",message="source is immutable; create a new SharedFilesystem instead"
type SharedFilesystemSpec struct {
	// source is the existing static PersistentVolume to distribute. Immutable.
	// +required
	Source FilesystemSource `json:"source"`

	// allowedNamespaces selects the namespaces whose SharedFilesystemClaims are accepted.
	// When omitted, no namespace is allowed.
	// +optional
	AllowedNamespaces *AllowedNamespaces `json:"allowedNamespaces,omitempty"`

	// maxAccess is the strongest access a claim may ask for. To offer read-only access to some
	// namespaces and read-write access to others, create two SharedFilesystems with the same
	// source.
	// +optional
	// +kubebuilder:default=ReadOnly
	MaxAccess Access `json:"maxAccess,omitempty"`
}

// ResolvedSource is the allow-listed copy of the source PersistentVolume that generated
// PersistentVolumes are built from.
type ResolvedSource struct {
	// uid of the source PersistentVolume; a change means the source was recreated.
	UID types.UID `json:"uid"`
	// driver is the CSI driver name.
	Driver string `json:"driver"`
	// volumeHandle of the source PersistentVolume.
	VolumeHandle string `json:"volumeHandle"`
	// volumeAttributes copied for the driver's known keys only.
	// +optional
	VolumeAttributes map[string]string `json:"volumeAttributes,omitempty"`
	// mountOptions of the source with any rw/ro option removed.
	// +optional
	// +listType=atomic
	MountOptions []string `json:"mountOptions,omitempty"`
	// capacity of the source PersistentVolume, as a quantity string.
	Capacity string `json:"capacity"`
}

// SharedFilesystemStatus is the observed state of a SharedFilesystem.
type SharedFilesystemStatus struct {
	// observedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// resolved is the source as last read. Claims use it only while SourceResolved is True and
	// observedGeneration equals metadata.generation.
	// +optional
	Resolved *ResolvedSource `json:"resolved,omitempty"`

	// boundClaims is the number of generated PersistentVolumes that currently point at this
	// file system.
	// +optional
	BoundClaims int32 `json:"boundClaims"`

	// conditions: SourceResolved, Ready, SourceReclaimDelete, Deleting.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sfs,categories=sharedfs
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.persistentVolumeName`
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.status.resolved.driver`
// +kubebuilder:printcolumn:name="Max",type=string,JSONPath=`.spec.maxAccess`
// +kubebuilder:printcolumn:name="Claims",type=integer,JSONPath=`.status.boundClaims`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SharedFilesystem offers one existing file system (through its static PersistentVolume) to the
// namespaces it selects. It is created by the platform admin.
type SharedFilesystem struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec SharedFilesystemSpec `json:"spec"`

	// +optional
	Status SharedFilesystemStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SharedFilesystemList contains a list of SharedFilesystem.
type SharedFilesystemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SharedFilesystem `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SharedFilesystem{}, &SharedFilesystemList{})
		return nil
	})
}
