/*
Copyright 2022.

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
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	GitRepositoryIndexKey     = ".metadata.gitRepository"
	MaxConditionMessageLength = 20000
)

// DockerBuildSpec defines the desired state of DockerBuild
type DockerBuildSpec struct {
	// The interval at which the instance will be reconciled.
	// +required
	Interval metav1.Duration `json:"interval"`

	// The interval at which to retry a previously failed reconciliation.
	// When not specified, the controller uses the CueInstanceSpec.Interval
	// value to retry failures.
	// +optional
	RetryInterval *metav1.Duration `json:"retryInterval,omitempty"`

	// A reference to a Flux Source from which an artifact will be downloaded
	// and the CUE instance built.
	// +required
	SourceRef CrossNamespaceSourceReference `json:"sourceRef"`

	// The spec of a container registry that the image should be pushed to
	// after build is successful.
	// +required
	ContainerRegistry ContainerRegistry `json:"containerRegistry"`
}

// DockerBuildStatus defines the observed state of DockerBuild
type DockerBuildStatus struct {
	meta.ReconcileRequestStatus `json:",inline"`

	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// The last successfully applied revision.
	// The revision format for Git sources is <branch|tag>/<commit-sha>.
	// +optional
	LastAppliedRevision string `json:"lastAppliedRevision,omitempty"`

	// LastAttemptedRevision is the revision of the last reconciliation attempt.
	// +optional
	LastAttemptedRevision string `json:"lastAttemptedRevision,omitempty"`
}

type ContainerRegistry struct {
	// The container repository that this build should push to
	// after build is finished.
	// +required
	Repository string `json:"repository"`

	// The image tagging strategy to use when the image
	// is pushed to the repository. The only option is 'commitSha'.
	// 'commitSha' uses the first 7 characters of the source commit
	// revision sha to tag the image with.
	// +kubebuilder:validation:Enum=commitSha
	// +required
	TagStrategy string `json:"tagStrategy"`

	// The credentials to use when pushing to the image repository.
	// +optional
	CredentialSecretRef *corev1.SecretReference `json:"credentialSecretRef,omitempty"`
}

// GetRetryInterval returns the retry interval
func (in DockerBuild) GetRetryInterval() time.Duration {
	if in.Spec.RetryInterval != nil {
		return in.Spec.RetryInterval.Duration
	}
	return in.Spec.Interval.Duration
}

// GetStatusConditions returns a pointer to the Status.Conditions slice.
func (in *DockerBuild) GetStatusConditions() *[]metav1.Condition {
	return &in.Status.Conditions
}

// DockerBuildProgressing resets the conditions of the given DockerBuild to a single
// ReadyCondition with status ConditionUnknown.
func DockerBuildProgressing(d DockerBuild, message string) DockerBuild {
	meta.SetResourceCondition(&d, meta.ReadyCondition, metav1.ConditionUnknown, meta.ProgressingReason, message)
	return d
}

func DockerBuildNotReady(k DockerBuild, revision, reason, message string) DockerBuild {
	SetDockerBuildReadiness(&k, metav1.ConditionFalse, reason, trimString(message, MaxConditionMessageLength), revision)
	if revision != "" {
		k.Status.LastAttemptedRevision = revision
	}
	return k
}

// SetDockerBuildReadiness sets the ReadyCondition, ObservedGeneration, and LastAttemptedRevision, on the DockerBuild.
func SetDockerBuildReadiness(k *DockerBuild, status metav1.ConditionStatus, reason, message string, revision string) {
	meta.SetResourceCondition(k, meta.ReadyCondition, status, reason, trimString(message, MaxConditionMessageLength))
	k.Status.ObservedGeneration = k.Generation
	k.Status.LastAttemptedRevision = revision
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// DockerBuild is the Schema for the dockerbuilds API
type DockerBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DockerBuildSpec   `json:"spec,omitempty"`
	Status DockerBuildStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DockerBuildList contains a list of DockerBuild
type DockerBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockerBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DockerBuild{}, &DockerBuildList{})
}

func trimString(str string, limit int) string {
	if len(str) <= limit {
		return str
	}

	return str[0:limit] + "..."
}
