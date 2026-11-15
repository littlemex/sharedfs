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

// Condition types and reasons are part of the API: tools and people match on them.
const (
	// ConditionReady is set on both kinds.
	ConditionReady = "Ready"

	// SharedFilesystem conditions.

	// ConditionSourceResolved is True when status.resolved reflects the source PersistentVolume.
	ConditionSourceResolved = "SourceResolved"
	// ConditionSourceReclaimDelete is True when the source PersistentVolume has reclaimPolicy
	// Delete: deleting the source's own PVC would delete the file system. Generated volumes are
	// Retain regardless. It is a warning and does not make the filesystem unusable.
	ConditionSourceReclaimDelete = "SourceReclaimDelete"
	// ConditionDeleting is True while deletion waits for generated volumes to go away.
	ConditionDeleting = "Deleting"

	// SharedFilesystemClaim conditions.

	// ConditionAccepted is True when the claim is allowed to mount the filesystem.
	ConditionAccepted = "Accepted"
	// ConditionBound mirrors whether the generated PersistentVolumeClaim is Bound.
	ConditionBound = "Bound"
)

const (
	ReasonResolved          = "Resolved"
	ReasonSourceNotFound    = "SourceNotFound"
	ReasonSourceNotCSI      = "SourceNotCSI"
	ReasonUnsupportedDriver = "UnsupportedDriver"
	ReasonSourceIsGenerated = "SourceIsGenerated"
	ReasonInvalidSource     = "InvalidSource"
	ReasonReclaimDelete     = "ReclaimPolicyDelete"
	ReasonReclaimRetain     = "ReclaimPolicyRetain"
	ReasonVolumesRemaining  = "VolumesRemaining"

	ReasonAccepted            = "Accepted"
	ReasonFilesystemNotFound  = "FilesystemNotFound"
	ReasonFilesystemNotReady  = "FilesystemNotReady"
	ReasonFilesystemDeleting  = "FilesystemDeleting"
	ReasonNamespaceNotAllowed = "NamespaceNotAllowed"
	ReasonAccessNotAllowed    = "AccessNotAllowed"
	ReasonPVCConflict         = "PVCConflict"
	ReasonSourceChanged       = "SourceChanged"
	ReasonBound               = "Bound"
	ReasonPending             = "Pending"
	ReasonPVCTerminating      = "PVCTerminating"
	ReasonReady               = "Ready"
	ReasonDeleting            = "Deleting"
)

// Revokes reports whether a rejection reason withdraws access: the generated PVC and PV are
// deleted. Every other rejection is treated as possibly temporary and leaves them in place, so a
// source PV that is briefly unreadable does not take every tenant's volume away.
func Revokes(reason string) bool {
	return reason == ReasonNamespaceNotAllowed || reason == ReasonAccessNotAllowed
}
