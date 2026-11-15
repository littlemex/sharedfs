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

// Condition types and reasons are part of the API: tools and people match on them. Every reason
// names one cause.
const (
	// ConditionReady is set on both kinds. On a SharedFilesystem it means the source resolves
	// and claims can be accepted. On a claim it means Accepted and Bound and nothing pending.
	ConditionReady = "Ready"
	// ConditionAccepted is set on claims: the namespace is granted the requested access, and
	// the filesystem and its source can be used.
	ConditionAccepted = "Accepted"
	// ConditionBound is set on claims and reports facts about the generated PVC and PV,
	// whether or not the claim is currently accepted.
	ConditionBound = "Bound"
)

// Reasons of SharedFilesystem Ready.
const (
	ReasonResolved          = "Resolved"
	ReasonSourceNotFound    = "SourceNotFound"
	ReasonSourceNotCSI      = "SourceNotCSI"
	ReasonUnsupportedDriver = "UnsupportedDriver"
	ReasonSourceIsGenerated = "SourceIsGenerated"
	ReasonInvalidSource     = "InvalidSource"
	ReasonInvalidGrant      = "InvalidGrant"
	ReasonVolumesRemaining  = "VolumesRemaining"
)

// Reasons of claim Accepted. The first three withdraw access when they persist past the
// revocation grace period; the others leave existing volumes in place.
const (
	ReasonAccepted            = "Accepted"
	ReasonNamespaceNotGranted = "NamespaceNotGranted"
	ReasonAccessNotGranted    = "AccessNotGranted"
	ReasonFilesystemNotFound  = "FilesystemNotFound"
	ReasonFilesystemDeleting  = "FilesystemDeleting"
	ReasonFilesystemNotReady  = "FilesystemNotReady"
)

// Reasons of claim Bound.
const (
	ReasonBound            = "Bound"
	ReasonNoVolume         = "NoVolume"
	ReasonPVCPending       = "PVCPending"
	ReasonPVCConflict      = "PVCConflict"
	ReasonPVCTerminating   = "PVCTerminating"
	ReasonPVCLost          = "PVCLost"
	ReasonPVCCreateFailed  = "PVCCreateFailed"
	ReasonVolumeTerminating = "VolumeTerminating"
)

// Reasons of claim Ready besides the ones it copies from Accepted and Bound.
const (
	ReasonReady         = "Ready"
	ReasonRevoking      = "Revoking"
	ReasonSourceChanged = "SourceChanged"
	ReasonDeleting      = "Deleting"
)

// Names sharedfs puts on the objects it manages.
const (
	// Finalizer is set on both kinds.
	Finalizer = "sharedfs.littlemex.github.io/cleanup"
	// LabelClaimUID marks a generated PersistentVolume or PersistentVolumeClaim with the UID of
	// its claim. Labels carry UIDs only: names can be longer than a label value allows.
	LabelClaimUID = "sharedfs.littlemex.github.io/claim-uid"
	// LabelFilesystemUID marks a generated PersistentVolume with the UID of its SharedFilesystem.
	LabelFilesystemUID = "sharedfs.littlemex.github.io/filesystem-uid"
	// AnnotationFilesystemName names the SharedFilesystem of a generated PersistentVolume.
	AnnotationFilesystemName = "sharedfs.littlemex.github.io/filesystem-name"
)
