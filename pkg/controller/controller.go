package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	batchinformers "k8s.io/client-go/informers/batch/v1"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	foov1 "github.com/dungdev1/foo-cronjob/pkg/apis/foo/v1"
	fooclientset "github.com/dungdev1/foo-cronjob/pkg/generated/clientset/versioned"
	informers "github.com/dungdev1/foo-cronjob/pkg/generated/informers/externalversions/foo/v1"
	listers "github.com/dungdev1/foo-cronjob/pkg/generated/listers/foo/v1"

	"github.com/robfig/cron"
)

const (
	GroupVersion string = "foo.com/v1"
	Kind         string = "CronJob"
	FooIndex     string = "ByFooCronjob"
	JobIndex     string = "ByJob"
)

const (
	AnnotationScheduleTime string = "foo.com/scheduled-at"
)

type workerID struct{}

type Controller struct {
	kubeclientset kubernetes.Interface
	fooclientset  fooclientset.Interface

	cjLister    listers.CronJobLister
	cjSynced    cache.InformerSynced
	jobInformer cache.SharedIndexInformer
	jobSynced   cache.InformerSynced

	queue workqueue.RateLimitingInterface

	initial map[string]struct{}
}

func NewController(
	kubeclientset kubernetes.Interface,
	fooclientset fooclientset.Interface,
	cjInformers informers.CronJobInformer,
	jobInformer batchinformers.JobInformer) *Controller {

	controller := &Controller{
		kubeclientset: kubeclientset,
		fooclientset:  fooclientset,
		cjLister:      cjInformers.Lister(),
		cjSynced:      cjInformers.Informer().HasSynced,

		jobInformer: jobInformer.Informer(),
		jobSynced:   jobInformer.Informer().HasSynced,
		queue:       workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),

		initial: make(map[string]struct{}),
	}

	cjInformers.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controller.enqueue(obj)
			// klog.Infof("Adding key %s to workqueue", key)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			// Restore groupVersionKind from oldObj. Checks issue https://github.com/kubernetes/kubernetes/issues/80609
			// gvk := oldObj.(*foov1.CronJob).GroupVersionKind()
			// newObj.(*foov1.CronJob).SetGroupVersionKind(gvk)
			// cjInformers.Informer().GetIndexer().Update(newObj)

			key := controller.enqueue(newObj)
			klog.V(1).Infof("Updating key %s to workqueue", key)
		},
		DeleteFunc: func(obj interface{}) {
			controller.enqueue(obj)
			// klog.Infof("Deleting key from workqueue")
		},
	})

	controller.jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controller.handleObj(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.handleObj(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			controller.handleObj(obj)
		},
	})

	controller.jobInformer.AddIndexers(map[string]cache.IndexFunc{
		FooIndex: func(obj interface{}) ([]string, error) {
			cronjobs := []string{}
			ownerRefs := obj.(*batchv1.Job).GetOwnerReferences()
			for _, ownerRef := range ownerRefs {
				if ownerRef.Kind == Kind && ownerRef.APIVersion == GroupVersion {
					cronjobs = append(cronjobs, obj.(*batchv1.Job).Namespace+"/"+ownerRef.Name)
				}
			}

			return cronjobs, nil
		},
	})

	return controller
}

func (c *Controller) handleObj(obj interface{}) {
	// Firstly, we need to check if this object implementing metav1.Object interface
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding tomstone object, invalid type"))
			return
		}
	}

	klog.V(1).Info("Processing object: ", object.GetName())
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		if ownerRef.APIVersion != GroupVersion || ownerRef.Kind != Kind {
			return
		}

		cronjob, err := c.cjLister.CronJobs(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			klog.InfoS("ignore orphan object", "object", object.GetName())
			return
		}
		// Enqueue the owner cronjob key for processing due to a dependent object change
		c.enqueue(cronjob)
	}
}
func (c *Controller) enqueue(obj interface{}) string {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return ""
	}
	c.queue.Add(key)
	return key
}

func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(ctx.Done(), c.cjSynced, c.jobSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Infof("Kubernetes controller has sync")

	for i := 0; i < workers; i++ {
		workerCtx := context.WithValue(ctx, workerID{}, i)
		go wait.UntilWithContext(workerCtx, c.runWorker, time.Second)
	}

	klog.Info("Started workers")
	<-ctx.Done()
	klog.Infof("Shutting down controller")
	return nil
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		fmt.Println("queue quit")
		return false
	}

	defer c.queue.Done(key)
	err := c.syncHandler(ctx, key.(string), time.Now())
	if err == nil {
		c.queue.Forget(key)
	} else {
		utilruntime.HandleError(err)
	}
	return true
}

func (c *Controller) syncHandler(ctx context.Context, key string, now time.Time) error {
	workerID := ctx.Value(workerID{}).(int)
	klog.V(1).InfoS("Processing item", "key", key, "worker", workerID)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return errors.Wrap(err, "invalid resource key")
	}

	cronjob, err := c.cjLister.CronJobs(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			klog.Errorf("CronJob %s does not exist anymore, ignore orphan object", key)
			delete(c.initial, key)
			return nil
		}
		return errors.Wrapf(err, "failed to get CronJob %s", key)
	}
	klog.V(1).InfoS("Process object", "cronjob", fmt.Sprintf("%s/%s", cronjob.Namespace, cronjob.Name))

	// 1. List all active jobs, and update the status (list of active jobs, last schedule time)
	jobs, err := c.jobInformer.GetIndexer().ByIndex(FooIndex, key)
	if err != nil {
		return errors.Wrap(err, "failed to get jobs")
	}

	activeJobs := []batchv1.Job{}
	failedJobs := []batchv1.Job{}
	successJobs := []batchv1.Job{}

	isJobFinished := func(job *batchv1.Job) (bool, batchv1.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}
		return false, ""
	}

	var lastScheduleTime *metav1.Time = nil

	for _, job := range jobs {
		j := job.(*batchv1.Job)
		_, finishedType := isJobFinished(j)
		switch finishedType {
		case batchv1.JobComplete:
			successJobs = append(successJobs, *j)
		case batchv1.JobFailed:
			failedJobs = append(failedJobs, *j)
		default:
			activeJobs = append(activeJobs, *j)
		}

		scheduleTime, err := getScheduleTime(j)
		if err != nil {
			klog.ErrorS(err, "failed to get schedule time", "job", j.Name)
			continue
		}

		if scheduleTime != nil {
			if lastScheduleTime == nil || lastScheduleTime.Before(scheduleTime) {
				lastScheduleTime = scheduleTime
			}
		}
	}

	cronJobStatus := &foov1.CronJobStatus{}

	for _, activeJob := range activeJobs {
		objRef, err := ref.GetReference(kubescheme.Scheme, &activeJob)
		if err != nil {
			klog.ErrorS(err, "get active job reference fail", "job", activeJob)
			continue
		}
		cronJobStatus.Active = append(cronJobStatus.Active, *objRef)
	}

	cronJobStatus.LastScheduleTime = lastScheduleTime

	// Number of jobs with type active, success, and fail
	// Active jobs not reflect the active job in CronJob status active field
	klog.InfoS("job count", "key", key, "active_jobs", len(activeJobs), "successful_jobs", len(successJobs), "failed_jobs", len(failedJobs), "last_schedule_time", fmt.Sprint(cronJobStatus.LastScheduleTime))

	updateFooCronJob := func(cronjob *foov1.CronJob, cronJobStatus *foov1.CronJobStatus) error {
		cronjobCopy := cronjob.DeepCopy()
		cronjobCopy.Status = *cronJobStatus.DeepCopy()
		klog.V(2).InfoS("update cronjob status", "workerID", workerID, "cronjob", fmt.Sprintf("%s/%s", cronjobCopy.Namespace, cronjobCopy.Name))
		_, err = c.fooclientset.FooV1().CronJobs(namespace).UpdateStatus(ctx, cronjobCopy, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to update cronjob status")
		}
		return nil
	}

	// If the last schedule time or the number of active jobs changed, update the cronjob status
	if cronJobStatus.LastScheduleTime != nil {
		if !cronjob.Status.LastScheduleTime.Equal(cronJobStatus.LastScheduleTime) || len(cronjob.Status.Active) != len(cronJobStatus.Active) {
			oldRV := cronjob.ResourceVersion
			// fmt.Println("old resoure version: ", oldRV)
			// fmt.Printf("old cronjob address: %p\n", cronjob)
			err = updateFooCronJob(cronjob, cronJobStatus)
			if err != nil {
				return err
			}
			// Need to wait until the resource has been synced with local cache
			for {
				newCJ, err := c.cjLister.CronJobs(cronjob.Namespace).Get(cronjob.Name)
				// fmt.Println("new resoure version: ", newCJ.ResourceVersion)
				// fmt.Printf("new cronjob address: %p\n", newCJ)
				if err != nil {
					klog.ErrorS(err, "failed to get newly updated job", "job", fmt.Sprintf("%s/%s", newCJ.Namespace, newCJ.Name))
					break
				}

				if newCJ.ResourceVersion != oldRV {
					break
				}

				// do not need to wait in case of testing
				if newCJ.ResourceVersion == "fortest01" {
					break
				}

				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	// 2. Clean old jobs according to the history limits
	var DeletionPolicy metav1.DeletionPropagation = metav1.DeletePropagationBackground

	sortJobs := func(jobs []batchv1.Job) {
		sort.Slice(jobs, func(i, j int) bool {
			t1, err := time.Parse(time.RFC3339, jobs[i].GetAnnotations()[AnnotationScheduleTime])
			if err != nil {
				klog.ErrorS(err, "cannot parse job schedule time", "job", jobs[i].Name)
				return false
			}
			t2, err := time.Parse(time.RFC3339, jobs[j].GetAnnotations()[AnnotationScheduleTime])
			if err != nil {
				klog.ErrorS(err, "cannot parse job schedule time", "job", jobs[i].Name)
				return false
			}
			return t1.After(t2)
		})
	}

	checkJobExist := func(job *batchv1.Job) bool {
		_, exists, _ := c.jobInformer.GetIndexer().GetByKey(strings.Join([]string{job.Namespace, job.Name}, "/"))
		return exists
	}

	if cronjob.Spec.SuccessfulJobsHistoryLimit != nil {
		if len(successJobs) > int(*cronjob.Spec.SuccessfulJobsHistoryLimit) {
			// sort history success job by time
			sortJobs(successJobs)
			for _, successJob := range successJobs[(*cronjob.Spec.SuccessfulJobsHistoryLimit):] {
				err := c.kubeclientset.BatchV1().Jobs(namespace).Delete(ctx, successJob.Name, metav1.DeleteOptions{PropagationPolicy: &DeletionPolicy})
				if err != nil {
					klog.ErrorS(err, "delete job failed", "job", successJob.Name)
					continue
				}
				klog.InfoS("deleted job successfuly", "job_condition", batchv1.JobComplete, "namespace", successJob.Namespace, "name", successJob.Name, "scheduled_time", successJob.GetAnnotations()[AnnotationScheduleTime])

				for {
					if exists := checkJobExist(&successJob); !exists {
						break
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
		}
	}

	if cronjob.Spec.FailedJobsHistoryLimit != nil {
		if len(failedJobs) > int(*cronjob.Spec.FailedJobsHistoryLimit) {
			// sort history failed job by time
			sortJobs(failedJobs)
			for _, failedJob := range failedJobs[(*cronjob.Spec.FailedJobsHistoryLimit):] {
				err := c.kubeclientset.BatchV1().Jobs(namespace).Delete(ctx, failedJob.Name, metav1.DeleteOptions{PropagationPolicy: &DeletionPolicy})
				if err != nil {
					klog.ErrorS(err, "delete job failed", "job", failedJob.Name)
					continue
				}
				klog.InfoS("deleted job successfuly", "job_condition", batchv1.JobFailed, "namespace", failedJob.Namespace, "name", failedJob.Name, "scheduled_time", failedJob.GetAnnotations()[AnnotationScheduleTime])

				for {
					if exists := checkJobExist(&failedJob); !exists {
						break
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
		}
	}

	// 3. Check if we're suspended the cronjob

	if cronjob.Spec.Suspend != nil && *cronjob.Spec.Suspend {
		klog.InfoS("Cronjob is suspended", "cronjob", fmt.Sprintf("%s/%s", cronjob.Namespace, cronjob.Name))
		return nil
	}

	// 4. Get the missed run and next schedule
	sched, err := cron.ParseStandard(cronjob.Spec.Schedule)
	if err != nil {
		return errors.Wrap(err, "parse cron schedule failed")
	}

	getNextSchedule := func(cronjob *foov1.CronJob, cronJobStatus foov1.CronJobStatus, now time.Time) (lastMissed time.Time, nextRun time.Time, err error) {
		var earliestTime time.Time
		if cronJobStatus.LastScheduleTime != nil {
			earliestTime = cronJobStatus.LastScheduleTime.Time
		} else {
			earliestTime = cronjob.CreationTimestamp.Time
		}

		for t := sched.Next(earliestTime); t.Before(now); t = sched.Next(t) {
			lastMissed = t
		}
		return lastMissed, sched.Next(now), nil
	}
	missedRun, nextRun, err := getNextSchedule(cronjob, *cronJobStatus, now)

	// 5. Run a new job if it's on schedule, not past the deadline and not blocked by concurrency policy
	if err != nil {
		return errors.Wrapf(err, "get next schedule failed, cronjob: %s/%s", cronjob.Namespace, cronjob.Name)
	}

	if missedRun.IsZero() {
		if _, ok := c.initial[key]; !ok {
			c.queue.AddAfter(key, time.Until(nextRun))
			c.initial[key] = struct{}{}
		}
		klog.InfoS("no upcoming scheduled times, sleeping until next", "key", key, "time", nextRun)
		return nil
	}

	// Check if the deadline has been exceeded
	if cronjob.Spec.StartingDeadlineSeconds != nil {
		if missedRun.Add(time.Duration(*cronjob.Spec.StartingDeadlineSeconds)).Before(now) {
			klog.InfoS("Deadline exceeded, skipping", "key", key, "deadline", missedRun.Add(time.Duration(*cronjob.Spec.StartingDeadlineSeconds)*time.Second))
			return nil
		}
	}

	if cronjob.Spec.ConcurrencyPolicy == "" || cronjob.Spec.ConcurrencyPolicy == foov1.AllowConcurrent {
		// default is AllowConcurrent
	} else if cronjob.Spec.ConcurrencyPolicy == foov1.ForbidConcurrent {
		if len(activeJobs) > 0 {
			klog.InfoS("Concurrency policy is ForbidConcurrent, skip this run", "cronjob", fmt.Sprintf("%s/%s", cronjob.Namespace, cronjob.Name))
			c.queue.AddAfter(key, time.Until(nextRun))
			return nil
		}
	} else if cronjob.Spec.ConcurrencyPolicy == foov1.ReplaceConcurrent {
		if len(activeJobs) > 0 {
			klog.InfoS("Concurrency policy is ReplaceConcurrent, delete all active jobs", "cronjob", fmt.Sprintf("%s/%s", cronjob.Namespace, cronjob.Name))
			for _, activeJob := range activeJobs {
				err := c.kubeclientset.BatchV1().Jobs(namespace).Delete(ctx, activeJob.Name, metav1.DeleteOptions{PropagationPolicy: &DeletionPolicy})
				if err != nil {
					klog.ErrorS(err, "delete job failed", "job", activeJob.Name)
					continue
				}
				klog.InfoS("Delete active job", "namespace", activeJob.Namespace, "name", activeJob.Name)
				time.Sleep(time.Second * 2)
			}
		}
	} else {
		return fmt.Errorf("invalid concurrency policy: %v", cronjob.Spec.ConcurrencyPolicy)
	}

	job := newJob(name, namespace, cronjob, missedRun, batchv1.JobStatus{})

	_, err = c.kubeclientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrapf(err, "create job failed, cronjob: %s/%s, missed run: %s", cronjob.Namespace, cronjob.Name, fmt.Sprint(missedRun))
	}
	klog.InfoS("created a new job for missed run", "time", missedRun, "job", fmt.Sprintf("%s/%s", job.Namespace, job.Name), "owner", cronjob.Name)

	// Check Sync success before continue
	for {
		if exist := checkJobExist(job); exist {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}

	// 6. Requeue the key after time until the next schedule run. We can use delaying queue for this purpose.
	c.queue.AddAfter(key, time.Until(nextRun))

	// TODO
	// klog.Info(obj)
	return nil
}

func getScheduleTime(job *batchv1.Job) (*metav1.Time, error) {
	timeString := job.Annotations[AnnotationScheduleTime]
	scheduleTime, err := time.Parse(time.RFC3339, timeString)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse schedule time")
	}
	return &metav1.Time{Time: scheduleTime}, nil
}

func newJob(name, namespace string, cronjob *foov1.CronJob, scheduleTime time.Time, status batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-" + fmt.Sprint(scheduleTime.Unix()),
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(cronjob, foov1.SchemeGroupVersion.WithKind(Kind)),
			},
			Annotations: map[string]string{
				AnnotationScheduleTime: scheduleTime.Format(time.RFC3339),
			},
		},
		Spec:   *(cronjob.Spec.JobTemplate.Spec).DeepCopy(),
		Status: status,
	}
}
