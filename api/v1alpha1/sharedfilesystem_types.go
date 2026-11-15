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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	return a == AccessReadWrite || want != AccessReadWrite
}

// PersistentVolumeReference names a PersistentVolume.
type PersistentVolumeReference struct {
	// name of the PersistentVolume.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// Grant lets the namespaces it selects mount the file system with at most the given access.
type Grant struct {
	// namespaceSelector matches namespace labels. An empty selector ({}) matches every
	// namespace. To name namespaces one by one, match kubernetes.io/metadata.name with In.
	// Tenants must not be able to edit the labels this selector reads.
	// +required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// access is the strongest mount the selected namespaces may ask for.
	// +optional
	// +kubebuilder:default=ReadOnly
	Access Access `json:"access,omitempty"`
}

// SharedFilesystemSpec offers one existing file system to the namespaces its grants select.
type SharedFilesystemSpec struct {
	// persistentVolumeRef names the existing static PersistentVolume that points at the file
	// system. Its CSI fields are copied into every generated PersistentVolume; the
	// PersistentVolume itself is only read, and its phase does not matter. Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="persistentVolumeRef is immutable; create a new SharedFilesystem instead"
	PersistentVolumeRef PersistentVolumeReference `json:"persistentVolumeRef"`

	// grants decide which namespaces may claim the file system. A namespace selected by
	// several grants gets the strongest access among them. With no grants, no namespace may.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	Grants []Grant `json:"grants,omitempty"`
}

// ResolvedSource shows what generated PersistentVolumes are built from. It is informational:
// the claim controller reads the source PersistentVolume itself.
type ResolvedSource struct {
	// driver is the CSI driver name.
	Driver string `json:"driver"`
	// volumeHandle of the source PersistentVolume.
	VolumeHandle string `json:"volumeHandle"`
	// volumeAttributes copied for the keys the driver reads.
	// +optional
	VolumeAttributes map[string]string `json:"volumeAttributes,omitempty"`
	// mountOptions of the source without any rw or ro option.
	// +optional
	// +listType=atomic
	MountOptions []string `json:"mountOptions,omitempty"`
	// capacity of the source PersistentVolume.
	Capacity resource.Quantity `json:"capacity"`
}

// SharedFilesystemStatus is the observed state of a SharedFilesystem.
type SharedFilesystemStatus struct {
	// observedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// resolved is the source as last read successfully.
	// +optional
	Resolved *ResolvedSource `json:"resolved,omitempty"`

	// volumes is the number of generated PersistentVolumes that point at this file system.
	// +optional
	Volumes int32 `json:"volumes"`

	// conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sfs,categories=sharedfs
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=`.spec.persistentVolumeRef.name`
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.status.resolved.driver`
// +kubebuilder:printcolumn:name="Volumes",type=integer,JSONPath=`.status.volumes`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SharedFilesystem offers one existing file system, through its static PersistentVolume, to the
// namespaces its grants select. It is created by the platform admin.
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
