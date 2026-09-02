/*
Copyright 2026 Enrico Bianchi.

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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceSpec defines the desired state of Instance
// +kubebuilder:validation:XValidation:rule="!self.databases.exists(d, d.shadow) || has(self.storage.shadow)",message="storage.shadow is required when any database has shadow enabled"
type InstanceSpec struct {
	// image is the container image used to run the Firebird instance.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// version is the Firebird version to deploy, used as the image tag.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// databases is the list of databases to create on the instance.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Databases []DatabaseSpec `json:"databases"`

	// service configures the Service exposing the instance.
	// +optional
	Service ServiceSpec `json:"service,omitzero"`

	// storage configures the persistent volumes backing the instance.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// authentication configures credentials for the instance.
	// +optional
	Authentication AuthenticationSpec `json:"authentication,omitzero"`
}

// DatabaseSpec defines a single database to create on the instance.
type DatabaseSpec struct {
	// name is the database file name, e.g. "instance.fdb".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// alias is the database alias clients use to connect.
	// Defaults to the database name if not specified.
	// +optional
	Alias *string `json:"alias,omitempty"`

	// shadow enables a shadow (mirror) database for this database.
	// Requires storage.shadow to be set.
	// +kubebuilder:default=false
	// +optional
	Shadow bool `json:"shadow,omitempty"`

	// pageSize is the database page size in bytes.
	// +kubebuilder:validation:Enum=4096;8192;16384
	// +kubebuilder:default=8192
	// +optional
	PageSize int32 `json:"pageSize,omitempty"`

	// charset is the database character set.
	// +kubebuilder:default="UTF8"
	// +optional
	Charset string `json:"charset,omitempty"`

	// collation is the database collation.
	// +kubebuilder:default="UTF8"
	// +optional
	Collation string `json:"collation,omitempty"`
}

// ServiceSpec configures the Service exposing the instance.
type ServiceSpec struct {
	// type is the Service type.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default="ClusterIP"
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`

	// port is the port the Service exposes the instance on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=3050
	// +optional
	Port int32 `json:"port,omitempty"`
}

// StorageSpec configures the persistent volumes backing the instance.
type StorageSpec struct {
	// primary is the volume storing the instance's primary databases.
	// +kubebuilder:validation:Required
	Primary StorageVolumeSpec `json:"primary"`

	// backup is the volume mounted into the instance for staging backups.
	// Optional; when omitted, no backup volume is created or mounted. When
	// set, deleting the Instance backs up every database into this volume
	// (via gbak) before removing the primary and shadow PVCs; the backup
	// PVC itself is never removed.
	// +optional
	Backup *StorageVolumeSpec `json:"backup,omitempty"`

	// shadow is the volume storing shadow databases. Required if any
	// database in spec.databases has shadow set to true.
	// +optional
	Shadow *StorageVolumeSpec `json:"shadow,omitempty"`
}

// StorageVolumeSpec configures a single persistent volume.
type StorageVolumeSpec struct {
	// class is the StorageClass name to use. If empty, the cluster's
	// default StorageClass is used.
	// +optional
	Class string `json:"class,omitempty"`

	// size is the requested volume size.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`
}

// AuthenticationSpec configures credentials for the instance.
type AuthenticationSpec struct {
	// sysdba configures the SYSDBA account credentials.
	// +optional
	Sysdba SysdbaAuthSpec `json:"sysdba,omitzero"`
}

// SysdbaAuthSpec configures the SYSDBA account credentials.
type SysdbaAuthSpec struct {
	// secretRef is the name of the Secret containing the SYSDBA
	// credentials. Defaults to "<instance-name>-sysdba" if not specified;
	// that Secret is created automatically with a generated password if
	// it doesn't already exist.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

// InstanceStatus defines the observed state of Instance.
type InstanceStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Instance resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// databases lists the databases from spec.databases that have
	// already been created on the instance, so they are not recreated
	// on subsequent reconciles.
	// +optional
	Databases []string `json:"databases,omitempty"`

	// databaseCount is the number of databases currently managed by the
	// instance, i.e. len(databases).
	// +optional
	DatabaseCount int32 `json:"databaseCount"`

	// sysdbaPasswordHash is a hash of the SYSDBA password Kubebird last
	// applied to the live server, used to detect when the referenced
	// Secret's password has been rotated.
	// +optional
	SysdbaPasswordHash string `json:"sysdbaPasswordHash,omitempty"`

	// phase is a high-level summary of the Instance's lifecycle state:
	// "Provisioning" while the StatefulSet or its databases aren't ready
	// yet, "Ready" once the pod is ready and every database in
	// spec.databases has been created, or "Deleting" while owned
	// resources are being garbage collected.
	// +optional
	Phase string `json:"phase,omitempty"`

	// error is the message from the most recent reconcile failure, if
	// any. Cleared automatically once the Instance reconciles
	// successfully again.
	// +optional
	Error string `json:"error,omitempty"`

	// message is a human-readable summary of the Instance's current
	// state: the reconcile error when the last reconcile failed,
	// otherwise a description of what phase it's in (e.g. why it isn't
	// Ready yet).
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Databases",type=integer,JSONPath=".status.databaseCount"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"

// Instance is the Schema for the instances API
type Instance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Instance
	// +required
	Spec InstanceSpec `json:"spec"`

	// status defines the observed state of Instance
	// +optional
	Status InstanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// InstanceList contains a list of Instance
type InstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Instance `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Instance{}, &InstanceList{})
		return nil
	})
}
