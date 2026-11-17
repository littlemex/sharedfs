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

// Condition types and reasons are part of the API. Every reason names one cause.
const (
	// ConditionReady is set on a FilesystemShare: new claims can be provisioned.
	ConditionReady = "Ready"
	// ConditionPrepared is set on a FilesystemShareDirectory: the directory exists as required.
	ConditionPrepared = "Prepared"
	// ConditionQuotaApplied is set on a FilesystemShareDirectory of a share with a quota: the
	// directory's project has the share's limits.
	ConditionQuotaApplied = "QuotaApplied"
)

// Reasons of FilesystemShare Ready.
const (
	ReasonResolved           = "Resolved"
	ReasonSourceNotFound     = "SourceNotFound"
	ReasonSourceNotCSI       = "SourceNotCSI"
	ReasonUnsupportedDriver  = "UnsupportedDriver"
	ReasonSourceIsGenerated  = "SourceIsGenerated"
	ReasonInvalidSource      = "InvalidSource"
	ReasonSourceUnreserved   = "SourceUnreserved"
	ReasonInvalidPath        = "InvalidPath"
	ReasonSubPathUnsupported = "SubPathUnsupported"
	ReasonInvalidGrant       = "InvalidGrant"
	ReasonUnsafeDriverPolicy = "UnsafeDriverPolicy"
	ReasonDriverNotInstalled = "DriverNotInstalled"
	ReasonOverlappingShare   = "OverlappingShare"
	ReasonStorageClassTaken  = "StorageClassTaken"
	ReasonVolumesRemaining   = "VolumesRemaining"
)

// Reasons of FilesystemShareDirectory Prepared.
const (
	ReasonDirectoryCreated   = "DirectoryCreated"
	ReasonDirectoryExists    = "DirectoryExists"
	ReasonPreparing          = "Preparing"
	ReasonPreparerTimedOut   = "PreparerTimedOut"
	ReasonInsecureParent     = "InsecureParent"
	ReasonNotADirectory      = "NotADirectory"
	ReasonPermissionDenied   = "PermissionDenied"
	ReasonPreparationFailed  = "PreparationFailed"
	ReasonPreparerNotCreated = "PreparerNotCreated"
)

// Reasons of FilesystemShareDirectory QuotaApplied.
const (
	ReasonQuotaApplied       = "QuotaApplied"
	ReasonQuotaPending       = "QuotaPending"
	ReasonQuotaNotConfigured = "QuotaNotConfigured"
	ReasonDirectoryNotTagged = "DirectoryNotTagged"
	ReasonQuotaFailed        = "QuotaFailed"
)

// Event reasons on PersistentVolumeClaims.
const (
	EventNotGranted         = "NotGranted"
	EventUnsupportedClaim   = "UnsupportedClaim"
	EventShareNotReady      = "ShareNotReady"
	EventNamespaceRecreated = "NamespaceRecreated"
	EventDirectoryNotReady  = "DirectoryNotReady"
	EventProvisioned        = "Provisioned"
	EventRevoking           = "Revoking"
	EventRevoked            = "Revoked"
	EventRevocationHeld     = "RevocationHeld"
)

// Names sharedfs puts on the objects it manages.
const (
	// Group is the API group, also the provisioner name of the StorageClasses.
	Group = "sharedfs.littlemex.github.io"
	// ProvisionerName is the provisioner of the StorageClasses a FilesystemShare owns.
	ProvisionerName = Group

	// Finalizer is set on FilesystemShares.
	Finalizer = Group + "/cleanup"

	// LabelShareUID marks generated PersistentVolumes, StorageClasses and directory records.
	LabelShareUID = Group + "/share-uid"
	// LabelPVCUID marks a generated PersistentVolume with the UID of its claim.
	LabelPVCUID = Group + "/pvc-uid"
	// LabelInternal marks the controller's own volumes and claims used to prepare directories.
	// They are never treated as tenant volumes.
	LabelInternal = Group + "/internal"

	// AnnotationShareName names the FilesystemShare of a generated object.
	AnnotationShareName = Group + "/share-name"
	// AnnotationAccess records on a generated PersistentVolume the access it was provisioned
	// with, so revocation does not depend on the StorageClass still existing.
	AnnotationAccess = Group + "/access"
	// AnnotationWithdrawAfter is set on a generated PersistentVolume, an object only admins
	// can edit, while its claim's namespace is not granted: the RFC 3339 time after which the
	// claim is deleted if the grant is still missing.
	AnnotationWithdrawAfter = Group + "/withdraw-after"
	// AnnotationPauseRevocation on a FilesystemShare holds every revocation of its claims, for
	// an admin who sees a grant change go wrong.
	AnnotationPauseRevocation = Group + "/pause-revocation"
	// AnnotationAllowOverlap on a FilesystemShare exempts it from the overlap guard, for an
	// admin's own root share.
	AnnotationAllowOverlap = Group + "/allow-overlap"

	// StorageClass parameters that tie a class to its share and access.
	ParameterShare    = "share"
	ParameterShareUID = "shareUID"
	ParameterAccess   = "access"
)
