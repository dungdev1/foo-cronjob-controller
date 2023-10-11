package v1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type ConcurrencyPolicy string

const (
	AllowConcurrent   = ConcurrencyPolicy("Allow")
	ForbidConcurrent  = ConcurrencyPolicy("Forbid")
	ReplaceConcurrent = ConcurrencyPolicy("Replace")
)

// CronJobSpec defines the desired state of CronJob
type CronJobSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster

	// The schedule in Cron format, see https://en.wikipedia.org/wiki/Cron.
	Schedule string `json:"schedule"`

	// Optional deadline in seconds for starting the job if it misses scheduled time for any reason.
	// Missed jobs executions will be counted as failed ones.
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// ConcurrencyPolicy specifies how to treat concurrent executions of a Job.
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// This flag tells the controller to suspend subsequent executions of this cron job. Defaults to false.
	Suspend *bool `json:"suspend,omitempty"`

	// JobTemplate is whole object description of the job that will be created when executing by Controller
	JobTemplate batchv1.JobTemplateSpec `json:"jobTemplate"`

	// Specifies the number of successful finished jobs to retain.
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`

	// Specifies the number of failed finished jobs to retain.
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
}

// CronJobStatus defines the observed state of CronJob
type CronJobStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster

	// A list of pointers to currently running jobs.
	Active []corev1.ObjectReference `json:"active,omitempty"`

	// The last time the job was successfully scheduled (not job status).
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CronJob is the Schema for the cronjobs API
type CronJob struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec CronJobSpec `json:"spec,omitempty"`

	Status CronJobStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type CronJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CronJob `json:"items"`
}
