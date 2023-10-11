package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	foov1 "github.com/dungdev1/foo-cronjob/pkg/apis/foo/v1"
	"github.com/dungdev1/foo-cronjob/pkg/generated/clientset/versioned/fake"
	"github.com/robfig/cron"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	kubefake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/diff"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	ref "k8s.io/client-go/tools/reference"

	informers "github.com/dungdev1/foo-cronjob/pkg/generated/informers/externalversions"
)

var (
	alwaysReady = func() bool { return true }
)

type fixture struct {
	t *testing.T

	// Fake clients to use in tests.
	client     *fake.Clientset
	kubeclient *kubefake.Clientset

	// These are the objects that are pre-created as if they are exists in the synced store.
	fooCronjobLister []*foov1.CronJob
	jobLister        []*batchv1.Job

	// These are expected actions we expect controller will output.
	kubeactions []core.Action
	actions     []core.Action

	kubeobjects []runtime.Object
	fooobjects  []runtime.Object
}

func newFixture(t *testing.T) *fixture {
	return &fixture{
		t:           t,
		kubeobjects: []runtime.Object{},
		fooobjects:  []runtime.Object{},
	}
}

func (f *fixture) newController(ctx context.Context) (*Controller, kubeinformers.SharedInformerFactory, informers.SharedInformerFactory) {
	// Create fake clientset, pass list of objects as arguments, these will be added to object tracker as existing objects
	f.client = fake.NewSimpleClientset(f.fooobjects...)
	f.kubeclient = kubefake.NewSimpleClientset(f.kubeobjects...)

	factory := informers.NewSharedInformerFactory(f.client, 0)
	kubeFactory := kubeinformers.NewSharedInformerFactory(f.kubeclient, 0)

	c := NewController(f.kubeclient, f.client, factory.Foo().V1().CronJobs(), kubeFactory.Batch().V1().Jobs())
	c.cjSynced = alwaysReady
	c.jobSynced = alwaysReady

	// Add prepared objects to cache
	for _, f := range f.fooCronjobLister {
		factory.Foo().V1().CronJobs().Informer().GetIndexer().Add(f)
	}

	for _, f := range f.jobLister {
		kubeFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(f)
	}

	return c, kubeFactory, factory
}

func (f *fixture) run(ctx context.Context, key string, initial bool, timeRun time.Time) {
	f.runController(ctx, key, false, initial, timeRun)
}

func (f *fixture) runExpectError(ctx context.Context, key string, initial bool, timeRun time.Time) {
	f.runController(ctx, key, true, initial, timeRun)
}

func (f *fixture) runController(ctx context.Context, key string, expectError bool, initial bool, timeRun time.Time) {
	c, kubeFactory, factory := f.newController(ctx)
	kubeFactory.Start(ctx.Done())
	factory.Start(ctx.Done())

	err := c.syncHandler(ctx, key, timeRun)
	if expectError && err == nil {
		f.t.Error("expected error syncing foo cronjob, got nil")
	} else if !expectError && err != nil {
		f.t.Errorf("error syncing foo cronjob: %v", err)
	}

	actualActions := filterInformerActions(f.client.Actions())
	for i, action := range actualActions {
		if len(f.actions) < (i + 1) {
			f.t.Errorf("%d unexpected actions: %+v", len(actualActions)-len(f.actions), actualActions[i:])
			break
		}

		checkAction(f.actions[i], action, f.t)
	}

	if len(f.actions) > len(actualActions) {
		f.t.Errorf("%d additional expected actions: %+v", len(f.actions)-len(actualActions), f.actions[len(actualActions):])
	}

	kubeActualActions := filterInformerActions(f.kubeclient.Actions())
	for i, action := range kubeActualActions {
		if len(f.kubeactions) < (i + 1) {
			f.t.Errorf("%d unexpected actions: %+v", len(kubeActualActions)-len(f.kubeactions), kubeActualActions[i:])
			break
		}
		checkAction(f.kubeactions[i], action, f.t)
	}

	if len(f.kubeactions) > len(kubeActualActions) {
		f.t.Errorf("%d additional expected actions: %+v", len(f.kubeactions)-len(kubeActualActions), f.kubeactions[len(kubeActualActions):])
	}
}

// checkAction verifies that expected and actual actions are equal and both have same attached resource
func checkAction(expected, actual core.Action, t *testing.T) {
	// Check expected action matchs actual action
	if !(expected.Matches(actual.GetVerb(), actual.GetResource().Resource) && expected.GetSubresource() == actual.GetSubresource()) {
		t.Errorf("Expected\n\t%+v\ngot\n\t%+v", expected, actual)
		return
	}

	// Check if two action have the same action type
	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		t.Errorf("Action has wrong type. Expected: %t. Got: %t", expected, actual)
		return
	}

	// Compare underlying object of both
	switch a := actual.(type) {
	case core.CreateActionImpl:
		e, _ := expected.(core.CreateActionImpl)
		expObject := e.GetObject()
		object := a.GetObject()
		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n%s",
				a.GetVerb(),
				a.GetResource().Resource,
				diff.ObjectGoPrintSideBySide(expObject, object))
		}
	case core.UpdateActionImpl:
		e, _ := expected.(core.UpdateActionImpl)
		expObject := e.GetObject()
		object := a.GetObject()
		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n%s",
				a.GetVerb(),
				a.GetResource().Resource,
				diff.ObjectGoPrintSideBySide(expObject, object))
		}
	case core.DeleteActionImpl:
		e, _ := expected.(core.DeleteActionImpl)
		expDeleteOptions := e.GetDeleteOptions()
		deleteOptions := a.GetDeleteOptions()
		if e.GetNamespace() != a.GetNamespace() || e.GetName() != a.GetName() || !reflect.DeepEqual(expDeleteOptions, deleteOptions) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n%s\n%s\n%s",
				a.GetVerb(),
				a.GetResource().Resource,
				diff.ObjectGoPrintSideBySide(e.GetNamespace(), a.GetNamespace()),
				diff.ObjectGoPrintSideBySide(e.GetName(), a.GetName()),
				diff.ObjectGoPrintSideBySide(expDeleteOptions, deleteOptions))
		}
	default:
		t.Errorf("Uncaptured Action %s %s, you should explicitly add a case to capture it",
			actual.GetVerb(), actual.GetResource().Resource)
	}
}

func filterInformerActions(actions []core.Action) []core.Action {
	ret := []core.Action{}
	for _, action := range actions {
		if len(action.GetNamespace()) == 0 &&
			(action.Matches("list", "cronjobs") ||
				action.Matches("watch", "cronjobs") ||
				action.Matches("list", "jobs") ||
				action.Matches("watch", "jobs")) {
			continue
		}
		ret = append(ret, action)
	}

	return ret
}

var ContainerRestartPolicyOnFailure corev1.ContainerRestartPolicy = "OnFailure"

func newCronjob(name string, schedule string, successfulJobLimit *int32, failedJobLimit *int32, concurentPolicy foov1.ConcurrencyPolicy, suspend *bool, deadline *int64, creationTime metav1.Time) *foov1.CronJob {
	return &foov1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: foov1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: creationTime,
		},
		Spec: foov1.CronJobSpec{
			Schedule:                   schedule,
			StartingDeadlineSeconds:    deadline,
			Suspend:                    suspend,
			ConcurrencyPolicy:          concurentPolicy,
			SuccessfulJobsHistoryLimit: successfulJobLimit,
			FailedJobsHistoryLimit:     failedJobLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:            "hello",
									Image:           "busybox:1.28",
									ImagePullPolicy: "IfNotPresent",
									Command:         []string{"/bin/sh", "-c", "date; echo Hello Thomas; echo sleep 30 seconds; sleep 30;"},
									RestartPolicy:   &ContainerRestartPolicyOnFailure,
								},
							},
						},
					},
				},
			},
		},
	}
}

func getKey(foo *foov1.CronJob, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(foo)
	if err != nil {
		t.Errorf("Unexpected error getting key for foo %v: %v", foo.Name, err)
		return ""
	}
	return key
}

func expectCreateJobAction(job *batchv1.Job) core.Action {
	return core.NewCreateAction(schema.GroupVersionResource{Resource: "jobs"}, job.GetNamespace(), job)
}

var DeletionPolicy metav1.DeletionPropagation = metav1.DeletePropagationBackground

func expectDeleteJobAction(job *batchv1.Job) core.Action {
	return core.NewDeleteActionWithOptions(schema.GroupVersionResource{Resource: "jobs"}, job.GetNamespace(), job.GetName(), metav1.DeleteOptions{PropagationPolicy: &DeletionPolicy})
}

func expectUpdateCronJobStatusAction(cronjob *foov1.CronJob) core.Action {
	return core.NewUpdateSubresourceAction(schema.GroupVersionResource{Resource: "cronjobs"}, "status", cronjob.GetNamespace(), cronjob)
}

func TestDoNoThing(t *testing.T) {
	now := time.Now()

	f := newFixture(t)
	fooCronjob := newCronjob("test", "* * * * *", nil, nil, foov1.AllowConcurrent, nil, nil, metav1.NewTime(now))
	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.run(workerCtx, getKey(fooCronjob, t), true, time.Now())
}

func TestCreateNewJob(t *testing.T) {
	now := time.Now()

	f := newFixture(t)
	fooCronjob := newCronjob("test", "* * * * *", nil, nil, foov1.AllowConcurrent, nil, nil, metav1.NewTime(now.Add(-time.Minute)))

	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	sched, _ := cron.ParseStandard("* * * * *")
	f.kubeactions = append(f.kubeactions, expectCreateJobAction(newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-time.Minute)), batchv1.JobStatus{})))

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.run(workerCtx, getKey(fooCronjob, t), false, now)
}

func TestWrongCronSchedule(t *testing.T) {
	now := time.Now()

	f := newFixture(t)
	fooCronjob := newCronjob("test", "* ** ** * * *", nil, nil, foov1.AllowConcurrent, nil, nil, metav1.NewTime(now))

	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.runExpectError(workerCtx, getKey(fooCronjob, t), true, time.Now())
}

// Need to handle failed jobs history limit
func TestJobHistoryReachLimit(t *testing.T) {
	now := time.Now()

	f := newFixture(t)

	fooCronjob := newCronjob("test", "* * * * *", int32Ptr(1), nil, foov1.AllowConcurrent, nil, nil, metav1.NewTime(now.Add(-10*time.Minute)))
	fooCronjob.ResourceVersion = "fortest01"
	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	sched, _ := cron.ParseStandard("* * * * *")

	old_job := newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-2*time.Minute)), batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{
			{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			},
		},
	})
	more_old_job := newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-3*time.Minute)), batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{
			{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			},
		},
	})
	f.jobLister = append(f.jobLister, old_job)
	f.jobLister = append(f.jobLister, more_old_job)
	f.kubeobjects = append(f.kubeobjects, old_job)
	f.kubeobjects = append(f.kubeobjects, more_old_job)

	expectFooCronJob := fooCronjob.DeepCopy()
	scheduleTime, err := getScheduleTime(old_job)
	if err != nil {
		t.FailNow()
	}

	expectFooCronJob.Status.LastScheduleTime = scheduleTime
	f.actions = append(f.kubeactions, expectUpdateCronJobStatusAction(expectFooCronJob))
	f.kubeactions = append(f.kubeactions, expectDeleteJobAction(more_old_job))
	f.kubeactions = append(f.kubeactions, expectCreateJobAction(newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-time.Minute)), batchv1.JobStatus{})))

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.run(workerCtx, getKey(fooCronjob, t), true, time.Now())
}

func TestDeadlineExceeded(t *testing.T) {
	now := time.Now()

	f := newFixture(t)

	fooCronjob := newCronjob("test", "* * * * *", int32Ptr(1), nil, foov1.AllowConcurrent, nil, int64Ptr(1), metav1.NewTime(now.Add(-10*time.Minute)))
	fooCronjob.ResourceVersion = "fortest01"
	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	sched, _ := cron.ParseStandard("* * * * *")

	old_job := newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-5*time.Minute)), batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{
			{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			},
		},
	})
	f.jobLister = append(f.jobLister, old_job)
	f.kubeobjects = append(f.kubeobjects, old_job)

	lastScheduleTime, _ := getScheduleTime(old_job)
	fooCronjob.Status.LastScheduleTime = lastScheduleTime

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.run(workerCtx, getKey(fooCronjob, t), true, now)
}

func TestAllowConcurentPolicy(t *testing.T) {
	now := time.Now()

	f := newFixture(t)

	fooCronjob := newCronjob("test", "* * * * *", int32Ptr(1), nil, foov1.AllowConcurrent, nil, nil, metav1.NewTime(now.Add(-10*time.Minute)))
	fooCronjob.ResourceVersion = "fortest01"
	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	sched, _ := cron.ParseStandard("* * * * *")

	old_job := newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-5*time.Minute)), batchv1.JobStatus{})
	f.jobLister = append(f.jobLister, old_job)
	f.kubeobjects = append(f.kubeobjects, old_job)

	lastScheduleTime, _ := getScheduleTime(old_job)
	fooCronjob.Status.LastScheduleTime = lastScheduleTime
	objRef, _ := ref.GetReference(kubescheme.Scheme, old_job)
	fooCronjob.Status.Active = append(fooCronjob.Status.Active, *objRef)

	f.kubeactions = append(f.kubeactions, expectCreateJobAction(newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-time.Minute)), batchv1.JobStatus{})))

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.run(workerCtx, getKey(fooCronjob, t), true, now)
}

func TestForbidConcurentPolicy(t *testing.T) {
	now := time.Now()

	f := newFixture(t)

	fooCronjob := newCronjob("test", "* * * * *", int32Ptr(1), nil, foov1.ForbidConcurrent, nil, nil, metav1.NewTime(now.Add(-10*time.Minute)))
	fooCronjob.ResourceVersion = "fortest01"
	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	sched, _ := cron.ParseStandard("* * * * *")

	old_job := newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-5*time.Minute)), batchv1.JobStatus{})
	f.jobLister = append(f.jobLister, old_job)
	f.kubeobjects = append(f.kubeobjects, old_job)

	lastScheduleTime, _ := getScheduleTime(old_job)
	fooCronjob.Status.LastScheduleTime = lastScheduleTime
	objRef, _ := ref.GetReference(kubescheme.Scheme, old_job)
	fooCronjob.Status.Active = append(fooCronjob.Status.Active, *objRef)

	// Expect no new job to be created
	// f.kubeactions = append(f.kubeactions, expectCreateJobAction(newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-time.Minute)), batchv1.JobStatus{})))

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.run(workerCtx, getKey(fooCronjob, t), true, now)
}

func TestReplaceConcurentPolicy(t *testing.T) {
	now := time.Now()

	f := newFixture(t)

	fooCronjob := newCronjob("test", "* * * * *", int32Ptr(1), nil, foov1.ReplaceConcurrent, nil, nil, metav1.NewTime(now.Add(-10*time.Minute)))
	fooCronjob.ResourceVersion = "fortest01"
	f.fooCronjobLister = append(f.fooCronjobLister, fooCronjob)
	f.fooobjects = append(f.fooobjects, fooCronjob)

	sched, _ := cron.ParseStandard("* * * * *")

	old_job := newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-5*time.Minute)), batchv1.JobStatus{})
	f.jobLister = append(f.jobLister, old_job)
	f.kubeobjects = append(f.kubeobjects, old_job)

	lastScheduleTime, _ := getScheduleTime(old_job)
	fooCronjob.Status.LastScheduleTime = lastScheduleTime
	objRef, _ := ref.GetReference(kubescheme.Scheme, old_job)
	fooCronjob.Status.Active = append(fooCronjob.Status.Active, *objRef)

	// Expect no new job to be created
	f.kubeactions = append(f.kubeactions, expectDeleteJobAction(old_job))
	f.kubeactions = append(f.kubeactions, expectCreateJobAction(newJob(fooCronjob.GetName(), fooCronjob.GetNamespace(), fooCronjob, sched.Next(now.Add(-time.Minute)), batchv1.JobStatus{})))

	workerCtx := context.WithValue(context.Background(), workerID{}, 0)
	f.run(workerCtx, getKey(fooCronjob, t), true, now)

}

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
