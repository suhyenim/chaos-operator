/*
Copyright 2019 LitmusChaos Authors

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

package controllers

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	litmuschaosv1alpha1 "github.com/litmuschaos/chaos-operator/api/litmuschaos/v1alpha1"
	"github.com/litmuschaos/chaos-operator/pkg/analytics"
	dynamicclientset "github.com/litmuschaos/chaos-operator/pkg/client/dynamic"
	chaosTypes "github.com/litmuschaos/chaos-operator/pkg/types"
	"github.com/litmuschaos/chaos-operator/pkg/utils"
	"github.com/litmuschaos/chaos-operator/pkg/utils/retry"
	"github.com/litmuschaos/elves/kubernetes/container"
	"github.com/litmuschaos/elves/kubernetes/pod"
	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const finalizer = "chaosengine.litmuschaos.io/finalizer"

// ChaosEngineReconciler reconciles a ChaosEngine object
type ChaosEngineReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client.Client
	// Used for serializing and deserializing API objects(group, version, and kind)
	Scheme *runtime.Scheme
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	Recorder record.EventRecorder
}

// reconcileEngine contains details of reconcileEngine
type reconcileEngine struct {
	r         *ChaosEngineReconciler
	reqLogger logr.Logger
}

// podEngineRunner contains the information of pod
type podEngineRunner struct {
	pod, engineRunner *corev1.Pod
	*reconcileEngine
}

//+kubebuilder:rbac:groups=litmuschaos.io,resources=chaosengines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=litmuschaos.io,resources=chaosengines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=litmuschaos.io,resources=chaosengines/finalizers,verbs=update

// Reconcile reads that state of the cluster for a ChaosEngine object and makes changes based on the state read
// and what is in the ChaosEngine.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue
func (r *ChaosEngineReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	reqLogger := startReqLogger(request)
	engine := &chaosTypes.EngineInfo{}

	if err := r.getChaosEngineInstance(engine, request); err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Handle deletion of ChaosEngine
	if engine.Instance.ObjectMeta.GetDeletionTimestamp() != nil {
		return r.reconcileForDelete(engine, request)
	}

	// Start the reconcile by setting default values into ChaosEngine
	if requeue, err := r.initEngine(engine); err != nil {
		if requeue {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}

	// Handling of normal execution of ChaosEngine
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateActive && engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusInitialized {
		return r.reconcileForCreationAndRunning(engine, *reqLogger)
	}

	// Handling Graceful completion of ChaosEngine
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateStop && engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusCompleted {
		return r.reconcileForComplete(engine, request)
	}

	// Handling forceful Abort of ChaosEngine
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateStop && engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusInitialized {
		return r.reconcileForDelete(engine, request)
	}

	// Handling restarting of ChaosEngine post Abort
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateActive && (engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusStopped) {
		return r.reconcileForRestartAfterAbort(engine, request)
	}

	// Handling restarting of ChaosEngine post Completion
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateActive && (engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusCompleted) {
		return r.reconcileForRestartAfterComplete(engine, request)
	}

	return ctrl.Result{}, nil
}

// getChaosRunnerENV return the env required for chaos-runner
func getChaosRunnerENV(engine *chaosTypes.EngineInfo, ClientUUID string) []corev1.EnvVar {

	var envDetails utils.ENVDetails
	envDetails.SetEnv("CHAOSENGINE", engine.Instance.Name).
		SetEnv("TARGETS", engine.Targets).
		SetEnv("EXPERIMENT_LIST", fmt.Sprint(strings.Join(engine.AppExperiments, ","))).
		SetEnv("CHAOS_SVC_ACC", engine.Instance.Spec.ChaosServiceAccount).
		SetEnv("AUXILIARY_APPINFO", engine.Instance.Spec.AuxiliaryAppInfo).
		SetEnv("CLIENT_UUID", ClientUUID).
		SetEnv("CHAOS_NAMESPACE", engine.Instance.Namespace)

	return envDetails.ENV
}

// getChaosRunnerLabels return the labels required for chaos-runner
func getChaosRunnerLabels(cr *litmuschaosv1alpha1.ChaosEngine) map[string]string {
	labels := map[string]string{
		"app":                         cr.Name,
		"chaosUID":                    string(cr.UID),
		"app.kubernetes.io/component": "chaos-runner",
		"app.kubernetes.io/part-of":   "litmus",
	}
	for k, v := range cr.Spec.Components.Runner.RunnerLabels {
		labels[k] = v
	}
	return labels
}

// newGoRunnerPodForCR defines a new go-based Runner Pod
func (r *ChaosEngineReconciler) newGoRunnerPodForCR(engine *chaosTypes.EngineInfo) (*corev1.Pod, error) {
	var experiment litmuschaosv1alpha1.ChaosExperiment
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: engine.Instance.Spec.Experiments[0].Name, Namespace: engine.Instance.Namespace}, &experiment); err != nil {
		return nil, err
	}

	engine.VolumeOpts.VolumeOperations(engine.Instance.Spec.Components.Runner.ConfigMaps, engine.Instance.Spec.Components.Runner.Secrets)

	containerForRunner := container.NewBuilder().
		WithEnvsNew(getChaosRunnerENV(engine, analytics.ClientUUID)).
		WithName("chaos-runner").
		WithImage(engine.Instance.Spec.Components.Runner.Image).
		WithImagePullPolicy(corev1.PullIfNotPresent)

	if engine.Instance.Spec.Components.Runner.ImagePullPolicy != "" {
		containerForRunner.WithImagePullPolicy(engine.Instance.Spec.Components.Runner.ImagePullPolicy)
	}

	if engine.Instance.Spec.Components.Runner.Args != nil {
		containerForRunner.WithArgumentsNew(engine.Instance.Spec.Components.Runner.Args)
	}

	if engine.VolumeOpts.VolumeMounts != nil {
		containerForRunner.WithVolumeMountsNew(engine.VolumeOpts.VolumeMounts)
	}

	if engine.Instance.Spec.Components.Runner.Command != nil {
		containerForRunner.WithCommandNew(engine.Instance.Spec.Components.Runner.Command)
	}

	if !reflect.DeepEqual(engine.Instance.Spec.Components.Runner.Resources, corev1.ResourceRequirements{}) {
		containerForRunner.WithResourceRequirements(engine.Instance.Spec.Components.Runner.Resources)
	}

	if !reflect.DeepEqual(experiment.Spec.Definition.SecurityContext.ContainerSecurityContext, corev1.SecurityContext{}) {
		containerForRunner.WithSecurityContext(experiment.Spec.Definition.SecurityContext.ContainerSecurityContext)
	}

	podForRunner := pod.NewBuilder().
		WithName(engine.Instance.Name + "-runner").
		WithNamespace(engine.Instance.Namespace).
		WithAnnotations(engine.Instance.Spec.Components.Runner.RunnerAnnotation).
		WithLabels(getChaosRunnerLabels(engine.Instance)).
		WithServiceAccountName(engine.Instance.Spec.ChaosServiceAccount).
		WithRestartPolicy("OnFailure").
		WithContainerBuilder(containerForRunner)

	if engine.Instance.Spec.Components.Runner.Tolerations != nil {
		podForRunner.WithTolerations(engine.Instance.Spec.Components.Runner.Tolerations...)
	}

	if len(engine.Instance.Spec.Components.Runner.NodeSelector) != 0 {
		podForRunner.WithNodeSelector(engine.Instance.Spec.Components.Runner.NodeSelector)
	}

	if engine.VolumeOpts.VolumeBuilders != nil {
		podForRunner.WithVolumeBuilders(engine.VolumeOpts.VolumeBuilders)
	}

	if engine.Instance.Spec.Components.Runner.ImagePullSecrets != nil {
		podForRunner.WithImagePullSecrets(engine.Instance.Spec.Components.Runner.ImagePullSecrets)
	}

	if !reflect.DeepEqual(experiment.Spec.Definition.SecurityContext.PodSecurityContext, corev1.PodSecurityContext{}) {
		podForRunner.WithSecurityContext(experiment.Spec.Definition.SecurityContext.PodSecurityContext)
	}

	runnerPod, err := podForRunner.Build()
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetControllerReference(engine.Instance, runnerPod, r.Scheme); err != nil {
		return nil, err
	}
	return runnerPod, nil
}

// engineRunnerPod to Check if the engineRunner pod already exists, else create
func engineRunnerPod(runnerPod *podEngineRunner) error {
	if err := runnerPod.r.Client.Get(context.TODO(), types.NamespacedName{Name: runnerPod.engineRunner.Name, Namespace: runnerPod.engineRunner.Namespace}, runnerPod.pod); err != nil && k8serrors.IsNotFound(err) {
		runnerPod.reqLogger.Info("Creating a new engineRunner Pod", "Pod.Namespace", runnerPod.engineRunner.Namespace, "Pod.Name", runnerPod.engineRunner.Name)
		if err = runnerPod.r.Client.Create(context.TODO(), runnerPod.engineRunner); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				runnerPod.reqLogger.Info("Skip reconcile: engineRunner Pod already exists", "Pod.Namespace", runnerPod.pod.Namespace, "Pod.Name", runnerPod.pod.Name)
				return nil
			}
			return err
		}

		// Pod created successfully - don't reconcile
		runnerPod.reqLogger.Info("engineRunner Pod created successfully")
		return nil
	} else if err != nil {
		return err
	}
	runnerPod.reqLogger.Info("Skip reconcile: engineRunner Pod already exists", "Pod.Namespace", runnerPod.pod.Namespace, "Pod.Name", runnerPod.pod.Name)
	return nil
}

// Fetch the ChaosEngine instance
func (r *ChaosEngineReconciler) getChaosEngineInstance(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	instance := &litmuschaosv1alpha1.ChaosEngine{}
	if err := r.Client.Get(context.TODO(), request.NamespacedName, instance); err != nil {
		// Error reading the object - reconcile the request.
		return err
	}
	engine.Instance = instance
	engine.AppInfo = instance.Spec.Appinfo
	engine.Selectors = instance.Spec.Selectors
	return nil
}

// Check if the engineRunner pod already exists, else create
func (r *ChaosEngineReconciler) checkEngineRunnerPod(engine *chaosTypes.EngineInfo, reqLogger logr.Logger) error {
	if len(engine.AppExperiments) == 0 {
		return errors.New("application experiment list is empty")
	}

	engineRunner, err := r.newGoRunnerPodForCR(engine)
	if err != nil {
		return err
	}

	// Create an object of engine reconcile.
	engineReconcile := &reconcileEngine{
		r:         r,
		reqLogger: reqLogger,
	}
	// Creates an object of engineRunner Pod
	runnerPod := &podEngineRunner{
		pod:             &corev1.Pod{},
		engineRunner:    engineRunner,
		reconcileEngine: engineReconcile,
	}

	return engineRunnerPod(runnerPod)
}

// setChaosResourceImage take the runner image from engine spec
// if it is not there then it will take from chaos-operator env
// at last if it is not able to find image in engine spec and operator env then it will take default images
func setChaosResourceImage(engine *chaosTypes.EngineInfo) {
	ChaosRunnerImage := os.Getenv("CHAOS_RUNNER_IMAGE")

	if engine.Instance.Spec.Components.Runner.Image == "" && ChaosRunnerImage == "" {
		engine.Instance.Spec.Components.Runner.Image = chaosTypes.DefaultChaosRunnerImage
	} else if engine.Instance.Spec.Components.Runner.Image == "" {
		engine.Instance.Spec.Components.Runner.Image = ChaosRunnerImage
	}
}

// reconcileForDelete reconciles for deletion/force deletion of Chaos Engine
func (r *ChaosEngineReconciler) reconcileForDelete(engine *chaosTypes.EngineInfo, request reconcile.Request) (reconcile.Result, error) {
	patch := client.MergeFrom(engine.Instance.DeepCopy())

	chaosTypes.Log.Info("Checking if there are any chaos resources to be deleted for", "chaosengine", engine.Instance.Name)

	chaosPodList := &corev1.PodList{}
	opts := []client.ListOption{
		client.InNamespace(request.NamespacedName.Namespace),
		client.MatchingLabels{"chaosUID": string(engine.Instance.UID)},
	}
	if err := r.Client.List(context.TODO(), chaosPodList, opts...); err != nil {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos stop) Unable to list chaos experiment pods")
		return reconcile.Result{}, err
	}

	if len(chaosPodList.Items) != 0 {
		chaosTypes.Log.Info("Performing a force delete of chaos experiment pods", "chaosengine", engine.Instance.Name)
		err := r.forceRemoveChaosResources(engine, request)
		if err != nil {
			r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos stop) Unable to delete chaos experiment pods")
			return reconcile.Result{}, err
		}
	}

	// update the chaos status in result for abort cases
	if err := r.updateChaosStatus(engine, request); err != nil {
		return reconcile.Result{}, err
	}

	if engine.Instance.ObjectMeta.Finalizers != nil {
		engine.Instance.ObjectMeta.Finalizers = utils.RemoveString(engine.Instance.ObjectMeta.Finalizers, "chaosengine.litmuschaos.io/finalizer")
	}

	// Update ChaosEngine ExperimentStatuses, with aborted Status.
	updateExperimentStatusesForStop(engine)
	engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusStopped

	if err := r.Client.Patch(context.TODO(), engine.Instance, patch); err != nil && !k8serrors.IsNotFound(err) {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos stop) Unable to update chaosengine")
		return reconcile.Result{}, fmt.Errorf("unable to remove finalizer from chaosEngine Resource, due to error: %v", err)
	}

	// we are repeating this condition/check here as we want the events for 'ChaosEngineStopped'
	// generated only after successful finalizer removal from the chaosengine resource
	if len(chaosPodList.Items) != 0 {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "ChaosEngineStopped", "Chaos resources deleted successfully")
	}

	return reconcile.Result{}, nil
}

// forceRemoveChaosResources force removes all chaos-related pods
func (r *ChaosEngineReconciler) forceRemoveChaosResources(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	optsDelete := []client.DeleteAllOfOption{client.InNamespace(request.NamespacedName.Namespace), client.MatchingLabels{"chaosUID": string(engine.Instance.UID)}, client.PropagationPolicy(v1.DeletePropagationBackground)}
	if engine.Instance.Spec.TerminationGracePeriodSeconds != 0 {
		optsDelete = append(optsDelete, client.GracePeriodSeconds(engine.Instance.Spec.TerminationGracePeriodSeconds))
	}

	var (
		deleteEvent []string
		err         []error
	)

	if errJob := r.Client.DeleteAllOf(context.TODO(), &batchv1.Job{}, optsDelete...); errJob != nil {
		err = append(err, errJob)
		deleteEvent = append(deleteEvent, "Jobs, ")
	}

	if errPod := r.Client.DeleteAllOf(context.TODO(), &corev1.Pod{}, optsDelete...); errPod != nil {
		err = append(err, errPod)
		deleteEvent = append(deleteEvent, "Pods, ")
	}
	if err != nil {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos stop) Unable to delete chaos resources: %v allocated to chaosengine", strings.Join(deleteEvent, ""))
		return fmt.Errorf("unable to delete ChaosResources due to %v", err)
	}

	return nil
}

// updateEngineState updates Chaos Engine Status with given State
func (r *ChaosEngineReconciler) updateEngineState(engine *chaosTypes.EngineInfo, state litmuschaosv1alpha1.EngineState) error {
	patch := client.MergeFrom(engine.Instance.DeepCopy())
	engine.Instance.Spec.EngineState = state

	if err := r.Client.Patch(context.TODO(), engine.Instance, patch); err != nil {
		return fmt.Errorf("unable to patch state of chaosEngine Resource, due to error: %v", err)
	}

	return nil
}

// checkRunnerContainerCompletedStatus check for the runner pod's container status for Completed
func (r *ChaosEngineReconciler) checkRunnerContainerCompletedStatus(engine *chaosTypes.EngineInfo) (bool, error) {
	runnerPod := corev1.Pod{}
	isCompleted := false

	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: engine.Instance.Name + "-runner", Namespace: engine.Instance.Namespace}, &runnerPod)
	if err != nil {
		return isCompleted, err
	}

	if runnerPod.Status.Phase == corev1.PodRunning || runnerPod.Status.Phase == corev1.PodSucceeded {
		for _, container := range runnerPod.Status.ContainerStatuses {
			if container.Name == "chaos-runner" && container.State.Terminated != nil {
				if container.State.Terminated.Reason == "Completed" {
					isCompleted = !container.Ready
				}
			}
		}
	}

	return isCompleted, nil
}

// gracefullyRemoveDefaultChaosResources removes all chaos-resources gracefully
func (r *ChaosEngineReconciler) gracefullyRemoveDefaultChaosResources(engine *chaosTypes.EngineInfo, request reconcile.Request) (reconcile.Result, error) {
	if engine.Instance.Spec.JobCleanUpPolicy == litmuschaosv1alpha1.CleanUpPolicyDelete {
		if err := r.gracefullyRemoveChaosPods(engine, request); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// gracefullyRemoveChaosPods removes chaos default resources gracefully
func (r *ChaosEngineReconciler) gracefullyRemoveChaosPods(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	optsList := []client.ListOption{
		client.InNamespace(request.NamespacedName.Namespace), client.MatchingLabels{"app": engine.Instance.Name, "chaosUID": string(engine.Instance.UID)},
	}

	var podList corev1.PodList
	if errList := r.Client.List(context.TODO(), &podList, optsList...); errList != nil {
		return errList
	}

	for _, v := range podList.Items {
		if errDel := r.Client.Delete(context.TODO(), &v, []client.DeleteOption{}...); errDel != nil {
			return errDel
		}
	}

	return nil
}

// reconcileForComplete reconciles for graceful completion of Chaos Engine
func (r *ChaosEngineReconciler) reconcileForComplete(engine *chaosTypes.EngineInfo, request reconcile.Request) (reconcile.Result, error) {
	if _, err := r.gracefullyRemoveDefaultChaosResources(engine, request); err != nil {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos completion) Unable to delete chaos pods upon chaos completion")
		return reconcile.Result{}, err
	}

	if err := r.updateEngineState(engine, litmuschaosv1alpha1.EngineStateStop); err != nil {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos completion) Unable to update chaosengine")
		return reconcile.Result{}, fmt.Errorf("unable to Update Engine State: %v", err)
	}

	return reconcile.Result{}, nil
}

// reconcileForRestartAfterAbort reconciles for restart of ChaosEngine after it was aborted previously
func (r *ChaosEngineReconciler) reconcileForRestartAfterAbort(engine *chaosTypes.EngineInfo, request reconcile.Request) (reconcile.Result, error) {
	if err := r.forceRemoveChaosResources(engine, request); err != nil {
		return reconcile.Result{}, err
	}

	if requeue, err := r.updateEngineForRestart(engine); err != nil {
		if requeue {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil

}

// reconcileForRestartAfterComplete reconciles for restart of ChaosEngine after it has completed successfully
func (r *ChaosEngineReconciler) reconcileForRestartAfterComplete(engine *chaosTypes.EngineInfo, request reconcile.Request) (reconcile.Result, error) {
	patch := client.MergeFrom(engine.Instance.DeepCopy())

	if err := r.forceRemoveChaosResources(engine, request); err != nil {
		return reconcile.Result{}, err
	}

	engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusInitialized
	engine.Instance.Status.Experiments = nil

	// finalizers have been retained in a completed chaosengine till this point (as chaos pods may be "retained")
	// as per the jobCleanUpPolicy. Stale finalizer is removed so that initEngine() generates the
	// ChaosEngineInitialized event and re-adds the finalizer before starting chaos.

	if engine.Instance.ObjectMeta.Finalizers != nil {
		engine.Instance.ObjectMeta.Finalizers = utils.RemoveString(engine.Instance.ObjectMeta.Finalizers, "chaosengine.litmuschaos.io/finalizer")
	}

	if err := r.Client.Patch(context.TODO(), engine.Instance, patch); err != nil {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos restart) Unable to update chaosengine")
		return reconcile.Result{}, fmt.Errorf("unable to patch state & remove stale finalizer in chaosEngine Resource, due to error: %v", err)
	}

	return reconcile.Result{}, nil
}

// initEngine initialize Chaos Engine, and add a finalizer to it.
func (r *ChaosEngineReconciler) initEngine(engine *chaosTypes.EngineInfo) (bool, error) {
	if engine.Instance.Spec.EngineState == "" {
		engine.Instance.Spec.EngineState = litmuschaosv1alpha1.EngineStateActive
	}

	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateActive && engine.Instance.Status.EngineStatus == "" {
		engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusInitialized
	}

	if engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusInitialized {
		if engine.Instance.ObjectMeta.Finalizers == nil {
			engine.Instance.ObjectMeta.Finalizers = append(engine.Instance.ObjectMeta.Finalizers, finalizer)
			if err := r.Client.Update(context.TODO(), engine.Instance, &client.UpdateOptions{}); err != nil {
				if k8serrors.IsConflict(err) {
					return true, err
				}
				return false, fmt.Errorf("unable to initialize ChaosEngine, because of Update Error: %v", err)
			}
			// generate the ChaosEngineInitialized event once finalizer has been added
			r.Recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "ChaosEngineInitialized", "Identifying app under test & launching %s", engine.Instance.Name+"-runner")
		}
	}

	return false, nil
}

// reconcileForCreationAndRunning reconciles for Chaos execution of Chaos Engine
func (r *ChaosEngineReconciler) reconcileForCreationAndRunning(engine *chaosTypes.EngineInfo, reqLogger logr.Logger) (reconcile.Result, error) {
	var runner corev1.Pod
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: engine.Instance.Name + "-runner", Namespace: engine.Instance.Namespace}, &runner); err != nil {
		if k8serrors.IsNotFound(err) {
			return r.createRunnerPod(engine, reqLogger)
		}
		return reconcile.Result{}, err
	}

	isCompleted, err := r.checkRunnerContainerCompletedStatus(engine)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos running) Unable to check chaos status")
		return reconcile.Result{}, err
	}

	if isCompleted {
		if requeue, err := r.updateEngineForComplete(engine, isCompleted); err != nil {
			if requeue {
				return reconcile.Result{Requeue: true}, nil
			}
			r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos completed) Unable to update chaos engine")
			return reconcile.Result{}, err
		}
	}

	reqLogger.Info("Skip reconcile: engineRunner Pod already exists", "Pod.Namespace", runner.Namespace, "Pod.Name", runner.Name)

	return reconcile.Result{}, nil
}

func (r *ChaosEngineReconciler) createRunnerPod(engine *chaosTypes.EngineInfo, reqLogger logr.Logger) (reconcile.Result, error) {
	if err := r.setExperimentDetails(engine); err != nil {
		if updateEngineErr := r.updateEngineState(engine, litmuschaosv1alpha1.EngineStateStop); updateEngineErr != nil {
			r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos stop) Unable to update chaosengine")
			return reconcile.Result{}, fmt.Errorf("unable to Update Engine State: %v", err)
		}
		return reconcile.Result{}, err
	}

	// Check if the engineRunner pod already exists, else create
	if err := r.checkEngineRunnerPod(engine, reqLogger); err != nil {
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "(chaos start) Unable to get chaos resources")
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *ChaosEngineReconciler) setExperimentDetails(engine *chaosTypes.EngineInfo) error {
	// Get the image for runner pod from chaosengine spec,operator env or default values.
	setChaosResourceImage(engine)

	if engine.Selectors != nil && engine.Selectors.Workloads == nil && engine.Selectors.Pods == nil {
		return fmt.Errorf("specify one out of workloads or pods")
	}

	if (engine.AppInfo.AppKind != "") != (engine.AppInfo.Applabel != "") {
		return fmt.Errorf("incomplete appinfo, provide appkind and applabel both")
	}

	engine.Targets = getTargets(engine)

	var appExperiments []string
	for _, exp := range engine.Instance.Spec.Experiments {
		appExperiments = append(appExperiments, exp.Name)
	}
	engine.AppExperiments = appExperiments

	chaosTypes.Log.Info("Targets derived from Chaosengine is ", "targets", engine.Targets)
	chaosTypes.Log.Info("Exp list derived from chaosengine is ", "appExpirements", appExperiments)
	chaosTypes.Log.Info("Runner image derived from chaosengine is", "runnerImage", engine.Instance.Spec.Components.Runner.Image)
	return nil
}

func getTargets(engine *chaosTypes.EngineInfo) string {
	if engine.Selectors == nil && reflect.DeepEqual(engine.AppInfo, litmuschaosv1alpha1.ApplicationParams{}) {
		return ""
	}

	var targets []string

	if engine.Selectors != nil {
		if engine.Selectors.Workloads != nil {
			for _, w := range engine.Selectors.Workloads {
				var filter string
				if w.Names != "" {
					filter = w.Names
				} else {
					filter = w.Labels
				}

				target := strings.Join([]string{string(w.Kind), w.Namespace, fmt.Sprintf("[%v]", filter)}, ":")
				targets = append(targets, target)
			}
			return strings.Join(targets, ";")
		}

		for _, w := range engine.Selectors.Pods {
			target := strings.Join([]string{"pod", w.Namespace, fmt.Sprintf("[%v]", w.Names)}, ":")
			targets = append(targets, target)
		}
		return strings.Join(targets, ";")
	}

	if engine.AppInfo.Appns == "" {
		engine.AppInfo.Appns = engine.Instance.Namespace
	}

	if engine.AppInfo.AppKind == "" {
		engine.AppInfo.AppKind = "KIND"
	}
	return strings.Join([]string{engine.AppInfo.AppKind, engine.AppInfo.Appns, fmt.Sprintf("[%v]", engine.AppInfo.Applabel)}, ":")
}

// updateExperimentStatusesForStop updates ChaosEngine.Status.Experiment with Abort Status.
func updateExperimentStatusesForStop(engine *chaosTypes.EngineInfo) {
	for i := range engine.Instance.Status.Experiments {
		if engine.Instance.Status.Experiments[i].Status == litmuschaosv1alpha1.ExperimentStatusRunning || engine.Instance.Status.Experiments[i].Status == litmuschaosv1alpha1.ExperimentStatusWaiting {
			engine.Instance.Status.Experiments[i].Status = litmuschaosv1alpha1.ExperimentStatusAborted
			engine.Instance.Status.Experiments[i].Verdict = "Stopped"
			engine.Instance.Status.Experiments[i].LastUpdateTime = v1.Now()
		}
	}
}

func startReqLogger(request reconcile.Request) *logr.Logger {
	reqLogger := chaosTypes.Log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ChaosEngine")

	return &reqLogger
}

func (r *ChaosEngineReconciler) updateEngineForComplete(engine *chaosTypes.EngineInfo, isCompleted bool) (bool, error) {
	if engine.Instance.Status.EngineStatus != litmuschaosv1alpha1.EngineStatusCompleted {
		engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusCompleted
		engine.Instance.Spec.EngineState = litmuschaosv1alpha1.EngineStateStop
		if err := r.Client.Update(context.TODO(), engine.Instance, &client.UpdateOptions{}); err != nil {
			if k8serrors.IsConflict(err) {
				return true, err
			}
			return false, fmt.Errorf("unable to update ChaosEngine Status, due to update error: %v", err)
		}
		r.Recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "ChaosEngineCompleted", "ChaosEngine completed, will delete or retain the resources according to jobCleanUpPolicy")
	}

	return false, nil
}

func (r *ChaosEngineReconciler) updateEngineForRestart(engine *chaosTypes.EngineInfo) (bool, error) {
	r.Recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "RestartInProgress", "ChaosEngine is restarted")
	engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusInitialized
	engine.Instance.Status.Experiments = nil
	if err := r.Client.Update(context.TODO(), engine.Instance, &client.UpdateOptions{}); err != nil {
		if k8serrors.IsConflict(err) {
			return true, err
		}
		return false, fmt.Errorf("unable to restart ChaosEngine, due to update error: %v", err)
	}

	return false, nil
}

// updateChaosStatus update the chaos status inside the chaosresult
func (r *ChaosEngineReconciler) updateChaosStatus(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	if err := r.waitForChaosPodTermination(engine, request); err != nil {
		return err
	}

	// skipping CRD validation for the namespace scoped operator
	if os.Getenv("WATCH_NAMESPACE") == "" {
		found, err := isResultCRDAvailable()
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
	}

	return r.updateChaosResult(engine, request)
}

// updateChaosResult update the chaosstatus and annotation inside the chaosresult
func (r *ChaosEngineReconciler) updateChaosResult(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	chaosresultList := &litmuschaosv1alpha1.ChaosResultList{}
	opts := []client.ListOption{
		client.InNamespace(request.NamespacedName.Namespace),
		client.MatchingLabels{},
	}

	if err := r.Client.List(context.TODO(), chaosresultList, opts...); err != nil {
		return err
	}

	for _, result := range chaosresultList.Items {
		if result.Labels["chaosUID"] == string(engine.Instance.UID) {
			if len(result.ObjectMeta.Annotations) == 0 {
				return nil
			}
			targetsList, annotations := getChaosStatus(result)
			result.Status.History.Targets = targetsList
			result.ObjectMeta.Annotations = annotations

			chaosTypes.Log.Info("updating chaos status inside chaosresult", "chaosresult", result.Name)
			return r.Client.Update(context.TODO(), &result, &client.UpdateOptions{})
		}
	}

	return nil
}

// waitForChaosPodTermination wait until the termination of chaos pod after abort
func (r *ChaosEngineReconciler) waitForChaosPodTermination(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	opts := []client.ListOption{
		client.InNamespace(request.NamespacedName.Namespace),
		client.MatchingLabels{"chaosUID": string(engine.Instance.UID)},
	}

	return retry.
		Times(uint(180)).
		Wait(1 * time.Second).
		Try(func(attempt uint) error {
			chaosPodList := &corev1.PodList{}
			if err := r.Client.List(context.TODO(), chaosPodList, opts...); err != nil {
				return err
			}
			if len(chaosPodList.Items) != 0 {
				return errors.Errorf("chaos pods are not deleted yet")
			}
			return nil
		})
}

// getChaosStatus return the target application details along with their chaos status
func getChaosStatus(result litmuschaosv1alpha1.ChaosResult) ([]litmuschaosv1alpha1.TargetDetails, map[string]string) {
	annotations := result.ObjectMeta.Annotations

	targetsList := result.Status.History.Targets
	for k, v := range annotations {
		switch strings.ToLower(v) {
		case "injected", "reverted", "targeted":
			kind := strings.TrimSpace(strings.Split(k, "/")[0])
			name := strings.TrimSpace(strings.Split(k, "/")[1])
			if !updateTargets(name, v, &targetsList) {
				targetsList = append(targetsList, litmuschaosv1alpha1.TargetDetails{
					Name:        name,
					Kind:        kind,
					ChaosStatus: v,
				})
			}
			delete(annotations, k)
		}
	}

	return targetsList, annotations
}

// isResultCRDAvailable check the existence of chaosresult CRD inside cluster
func isResultCRDAvailable() (bool, error) {

	dynamicClient, err := dynamicclientset.CreateClientSet()
	if err != nil {
		return false, err
	}

	// defining the gvr for the requested resource
	gvr := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}

	resultList, err := dynamicClient.Resource(gvr).List(context.Background(), v1.ListOptions{})
	if err != nil {
		return false, err
	}

	// check the presence of chaosresult CRD inside cluster
	for _, crd := range resultList.Items {
		if crd.GetName() == chaosTypes.ResultCRDName {
			return true, nil
		}
	}

	return false, nil
}

// updates the chaos status of targets which is already present inside history.targets
func updateTargets(name, status string, data *[]litmuschaosv1alpha1.TargetDetails) bool {
	for i := range *data {
		if (*data)[i].Name == name {
			(*data)[i].ChaosStatus = status
			return true
		}
	}

	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *ChaosEngineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&litmuschaosv1alpha1.ChaosEngine{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
