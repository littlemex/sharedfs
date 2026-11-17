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

// Access is the kind of mount a namespace may get.
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

// Grant lets the namespaces it selects claim the share with at most the given access.
// +kubebuilder:validation:XValidation:rule="(has(self.namespaceSelector.matchLabels) && size(self.namespaceSelector.matchLabels) > 0) || (has(self.namespaceSelector.matchExpressions) && size(self.namespaceSelector.matchExpressions) > 0)",message="namespaceSelector must not be empty; to grant every namespace, match kubernetes.io/metadata.name with operator Exists"
type Grant struct {
	// namespaceSelector matches namespace labels and must not be empty, so a selector whose
	// fields were dropped by a typo does not grant every namespace. Tenants must not be able to
	// edit the labels it reads.
	// +required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// access is the strongest mount the selected namespaces may claim.
	// +required
	Access Access `json:"access"`
}

// Provision asks the controller to create the share's directory when it is missing. An
// existing directory is never modified, and nothing is ever deleted.
type Provision struct {
	// uid owns the created directory.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	UID int64 `json:"uid"`

	// gid is the group of the created directory. Pods write through it, for example with
	// runAsGroup or supplementalGroups.
	// +required
	// +kubebuilder:validation:Minimum=0
	GID int64 `json:"gid"`

	// mode of the created directory, octal; the setgid bit (2770) makes new files inherit gid.
	// setuid and sticky are not allowed.
	// +optional
	// +kubebuilder:default="2770"
	// +kubebuilder:validation:Pattern=`^0?[02]?[0-7]{3}$`
	Mode string `json:"mode"`

	// quota limits each created directory with a Lustre project quota. The directory gets a
	// project ID of its own when it is created, inherited by everything below it; the limits may
	// change later. Needs Lustre 2.15 and the controller's quota pods (--quota-image).
	// +optional
	Quota *Quota `json:"quota,omitempty"`
}

// Quota is the hard limit of one directory.
// +kubebuilder:validation:XValidation:rule="has(self.storage) || has(self.inodes)",message="set storage, inodes or both"
type Quota struct {
	// storage is the most data the directory may hold.
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
	// inodes is the most files and directories it may hold.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Inodes *int64 `json:"inodes,omitempty"`
}

// FilesystemShareSpec offers one path of an existing file system to the namespaces its grants
// select. The whole spec except grants is immutable.
// +kubebuilder:validation:XValidation:rule="self.persistentVolumeRef == oldSelf.persistentVolumeRef && self.path == oldSelf.path && (has(self.provision) == has(oldSelf.provision)) && (!has(self.provision) || (self.provision.uid == oldSelf.provision.uid && self.provision.gid == oldSelf.provision.gid && self.provision.mode == oldSelf.provision.mode))",message="only grants and quota limits can change; create a new FilesystemShare instead"
type FilesystemShareSpec struct {
	// persistentVolumeRef names the existing static PersistentVolume of the file system. It is
	// only read. It must be bound or reserved with claimRef, otherwise any namespace could bind
	// it directly.
	// +required
	PersistentVolumeRef PersistentVolumeReference `json:"persistentVolumeRef"`

	// path inside the file system that the share exposes: "/" for the root, or a relative path
	// such as "datasets" or "checkpoints/{namespace}". "{namespace}" takes a whole segment and
	// expands to the namespace of the claiming PersistentVolumeClaim. Drivers without
	// sub-directory mounts accept only "/".
	// +required
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="self == '/' || (self.matches('^([{]namespace[}]|[A-Za-z0-9_][A-Za-z0-9._-]*)(/([{]namespace[}]|[A-Za-z0-9_][A-Za-z0-9._-]*))*$') && !self.matches('[^/]{256}') && !self.matches('^([^/]*/){32}'))",message="path must be \"/\" or relative segments of [A-Za-z0-9._-] (not starting with \".\" or \"-\") or \"{namespace}\""
	Path string `json:"path"`

	// grants decide which namespaces may claim the share. A claim is provisioned only when a
	// single grant selects its namespace and allows the access of the StorageClass it uses.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	Grants []Grant `json:"grants,omitempty"`

	// provision creates the directory when a granted namespace first claims the share.
	// Without it the directory must already exist; a missing one fails the mount.
	// +optional
	Provision *Provision `json:"provision,omitempty"`
}

// ResolvedSource shows what generated volumes are built from.
type ResolvedSource struct {
	Driver       string `json:"driver"`
	VolumeHandle string `json:"volumeHandle"`
	// +optional
	VolumeAttributes map[string]string `json:"volumeAttributes,omitempty"`
	// +optional
	// +listType=atomic
	MountOptions []string          `json:"mountOptions,omitempty"`
	Capacity     resource.Quantity `json:"capacity"`
}

// StorageClassNames are the classes tenants put in storageClassName.
type StorageClassNames struct {
	ReadWrite string `json:"readWrite"`
	ReadOnly  string `json:"readOnly"`
}

// FilesystemShareStatus is the observed state of a FilesystemShare.
type FilesystemShareStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Resolved *ResolvedSource `json:"resolved,omitempty"`
	// storageClasses tenants claim the share through.
	// +optional
	StorageClasses *StorageClassNames `json:"storageClasses,omitempty"`
	// volumes is the number of PersistentVolumes provisioned from this share.
	// +optional
	Volumes int32 `json:"volumes"`
	// readWriteSince is when the controller first saw a ReadWrite grant on this share, cleared
	// when none remains. Of two nested shares, the one that brought the conflict later gives way:
	// an outer share by this time, an inner share by its creation.
	// +optional
	ReadWriteSince *metav1.Time `json:"readWriteSince,omitempty"`
	// conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fss,categories=sharedfs
// +kubebuilder:validation:XValidation:rule="!has(self.spec.provision) || (self.spec.path != '/' && self.spec.path.contains('/'))",message="provision needs a path of two segments or more: the preparer mounts the parent read-write, and the parent of a top-level directory is the whole file system"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 250",message="name must be at most 250 characters: the StorageClasses are named <name>-rw and <name>-ro"
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=`.spec.persistentVolumeRef.name`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="RW-Class",type=string,JSONPath=`.status.storageClasses.readWrite`,priority=1
// +kubebuilder:printcolumn:name="RO-Class",type=string,JSONPath=`.status.storageClasses.readOnly`,priority=1
// +kubebuilder:printcolumn:name="Volumes",type=integer,JSONPath=`.status.volumes`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FilesystemShare offers one path of an existing file system to the namespaces its grants
// select. Tenants claim it with an ordinary PersistentVolumeClaim of one of its two
// StorageClasses. It is created by the platform admin.
type FilesystemShare struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec FilesystemShareSpec `json:"spec"`
	// +optional
	Status FilesystemShareStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FilesystemShareList contains a list of FilesystemShare.
type FilesystemShareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FilesystemShare `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FilesystemShare{}, &FilesystemShareList{})
		return nil
	})
}
