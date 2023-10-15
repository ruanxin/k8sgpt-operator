/*
Copyright 2023 The K8sGPT Authors.
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
	"strings"
	"time"

	corev1alpha1 "github.com/k8sgpt-ai/k8sgpt-operator/api/v1alpha1"
	"github.com/k8sgpt-ai/k8sgpt-operator/pkg/backend"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kclient "github.com/k8sgpt-ai/k8sgpt-operator/pkg/client"
	"github.com/k8sgpt-ai/k8sgpt-operator/pkg/integrations"
	"github.com/k8sgpt-ai/k8sgpt-operator/pkg/resources"
	"github.com/k8sgpt-ai/k8sgpt-operator/pkg/sinks"
	"github.com/k8sgpt-ai/k8sgpt-operator/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	FinalizerName            = "k8sgpt.ai/finalizer"
	ReconcileErrorInterval   = 10 * time.Second
	ReconcileSuccessInterval = 30 * time.Second
	fieldOwner               = "ai-sre.kyma-project.io/owner"
)

var (
	// Metrics
	// k8sgptReconcileErrorCount is a metric for the number of errors during reconcile
	k8sgptReconcileErrorCount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "k8sgpt_reconcile_error_count",
		Help: "The total number of errors during reconcile",
	})
	// k8sgptNumberOfResults is a metric for the number of results
	k8sgptNumberOfResults = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "k8sgpt_number_of_results",
		Help: "The total number of results",
	})
	// k8sgptNumberOfResultsByType is a metric for the number of results by type
	k8sgptNumberOfResultsByType = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8sgpt_number_of_results_by_type",
		Help: "The total number of results by type",
	}, []string{"kind", "name"})
)

// K8sGPTReconciler reconciles a K8sGPT object
type K8sGPTReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Integrations    *integrations.Integrations
	SinkClient      *sinks.Client
	K8sGPTClient    *kclient.Client
	TokenExpireTime time.Duration
	record.EventRecorder
}

// +kubebuilder:rbac:groups=core.k8sgpt.ai,resources=k8sgpts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.k8sgpt.ai,resources=k8sgpts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.k8sgpt.ai,resources=k8sgpts/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.k8sgpt.ai,resources=results,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete

func (r *K8sGPTReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// Look up the instance for this reconcile request
	k8sgptConfig := &corev1alpha1.K8sGPT{}
	err := r.Get(ctx, req.NamespacedName, k8sgptConfig)
	if err != nil {
		// Error reading the object - requeue the request.
		k8sgptReconcileErrorCount.Inc()
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return r.processState(ctx, k8sgptConfig)

}

func (r *K8sGPTReconciler) processState(ctx context.Context, k8sgptConfig *corev1alpha1.K8sGPT) (ctrl.Result, error) {
	switch k8sgptConfig.Status.State {
	case "":
		return r.handleInitialState(ctx, k8sgptConfig)
	case corev1alpha1.StateDeleting:
		return r.handleDeletingState(ctx, k8sgptConfig)
	default:
		return r.handleProcessingState(ctx, k8sgptConfig)
	}
}

func (r *K8sGPTReconciler) handleProcessingState(ctx context.Context, k8sgptConfig *corev1alpha1.K8sGPT) (ctrl.Result, error) {
	err := r.reconcile(ctx, k8sgptConfig)
	if err != nil {
		return ctrl.Result{Requeue: true}, r.setStatus(ctx, k8sgptConfig, corev1alpha1.StateError, err.Error())
	}

	if k8sgptConfig.Status.State == corev1alpha1.StateReady || k8sgptConfig.Status.State == corev1alpha1.StateWarning {
		return ctrl.Result{RequeueAfter: ReconcileSuccessInterval}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *K8sGPTReconciler) handleInitialState(ctx context.Context, k8sgptConfig *corev1alpha1.K8sGPT) (ctrl.Result, error) {
	if err := r.setStatus(ctx, k8sgptConfig, corev1alpha1.StateProcessing, "started processing"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *K8sGPTReconciler) handleDeletingState(ctx context.Context, k8sgptConfig *corev1alpha1.K8sGPT) (ctrl.Result, error) {
	// The object is being deleted
	if utils.ContainsString(k8sgptConfig.GetFinalizers(), FinalizerName) {

		// Delete any external resources associated with the instance
		err := resources.Sync(ctx, r.Client, *k8sgptConfig, resources.DestroyOp, nil)
		if err != nil {
			k8sgptReconcileErrorCount.Inc()
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(k8sgptConfig, FinalizerName)
		if err := r.Update(ctx, k8sgptConfig); err != nil {
			k8sgptReconcileErrorCount.Inc()
			return ctrl.Result{}, err
		}
	}
	// Stop reconciliation as the item is being deleted
	return ctrl.Result{}, nil
}

func (r *K8sGPTReconciler) reconcile(ctx context.Context,
	k8sgptConfig *corev1alpha1.K8sGPT) error {

	if !k8sgptConfig.DeletionTimestamp.IsZero() && k8sgptConfig.Status.State != corev1alpha1.StateDeleting {
		return r.setStatus(ctx, k8sgptConfig, corev1alpha1.StateDeleting, "waiting for modules to be deleted")
	}

	// Add a finaliser if there isn't one
	if k8sgptConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !utils.ContainsString(k8sgptConfig.GetFinalizers(), FinalizerName) {
			controllerutil.AddFinalizer(k8sgptConfig, FinalizerName)
			if err := r.Update(ctx, k8sgptConfig); err != nil {
				k8sgptReconcileErrorCount.Inc()
				return err
			}
		}
	}

	// Check if ServiceBinding secret exists
	backendInfo, err := backend.GetBackendInfo(ctx, r.Client, k8sgptConfig, r.TokenExpireTime)
	if err != nil {
		return err
	}
	// Check and see if the instance is new or has a K8sGPT deployment in flight
	deployment := v1.Deployment{}
	err = r.Get(ctx, client.ObjectKey{Namespace: k8sgptConfig.Namespace,
		Name: resources.DeploymentName}, &deployment)
	if client.IgnoreNotFound(err) != nil {
		k8sgptReconcileErrorCount.Inc()
		return err
	}
	err = resources.Sync(ctx, r.Client, *k8sgptConfig, resources.SyncOp, backendInfo)
	if err != nil {
		k8sgptReconcileErrorCount.Inc()
		return err
	}

	if deployment.Status.ReadyReplicas == 0 {
		return r.setStatus(ctx, k8sgptConfig, corev1alpha1.StateProcessing, "waiting for deployment ready")
	}

	// Check the version of the deployment image matches the version set in the K8sGPT CR
	imageURI := deployment.Spec.Template.Spec.Containers[0].Image
	imageVersion := strings.Split(imageURI, ":")[1]
	if imageVersion != k8sgptConfig.Spec.Version {
		// Update the deployment image
		deployment.Spec.Template.Spec.Containers[0].Image = fmt.Sprintf("%s:%s",
			strings.Split(imageURI, ":")[0], k8sgptConfig.Spec.Version)
		err = r.Update(ctx, &deployment)
		if err != nil {
			k8sgptReconcileErrorCount.Inc()
			return err
		}

		return r.setStatus(ctx, k8sgptConfig, corev1alpha1.StateProcessing, "update deployment image")
	}

	// If the deployment is active, we will query it directly for analysis data
	address, err := kclient.GenerateAddress(ctx, r.Client, k8sgptConfig)
	if err != nil {
		k8sgptReconcileErrorCount.Inc()
		return err
	}
	// Log address
	fmt.Printf("K8sGPT address: %s\n", address)

	k8sgptClient, err := kclient.NewClient(address)
	if err != nil {
		k8sgptReconcileErrorCount.Inc()
		return err
	}

	defer k8sgptClient.Close()

	// Configure the k8sgpt deployment if required
	if k8sgptConfig.Spec.RemoteCache != nil {
		err = k8sgptClient.AddConfig(k8sgptConfig)
		if err != nil {
			k8sgptReconcileErrorCount.Inc()
			return err
		}
	}

	response, err := k8sgptClient.ProcessAnalysis(deployment, k8sgptConfig)
	if err != nil {
		k8sgptReconcileErrorCount.Inc()
		return err
	}
	// Parse the k8sgpt-deployment response into a list of results
	k8sgptNumberOfResults.Set(float64(len(response.Results)))
	rawResults, err := resources.MapResults(*r.Integrations, response.Results, *k8sgptConfig)
	if err != nil {
		k8sgptReconcileErrorCount.Inc()
		return err
	}
	// Prior to creating or updating any results we will delete any stale results that
	// no longer are relevent, we can do this by using the resultSpec composed name against
	// the custom resource name
	resultList := &corev1alpha1.ResultList{}
	err = r.List(ctx, resultList)
	if err != nil {
		k8sgptReconcileErrorCount.Inc()
		return err
	}
	if len(resultList.Items) > 0 {
		// If the result does not exist in the map we will delete it
		for _, result := range resultList.Items {
			fmt.Printf("Checking if %s is still relevant\n", result.Name)
			if _, ok := rawResults[result.Name]; !ok {
				err = r.Delete(ctx, &result)
				if err != nil {
					k8sgptReconcileErrorCount.Inc()
					return err
				} else {
					k8sgptNumberOfResultsByType.With(prometheus.Labels{
						"kind": result.Spec.Kind,
						"name": result.Name,
					}).Dec()
				}
			}
		}
	}
	// At this point we are able to loop through our rawResults and create them or update
	// them as needed
	for _, result := range rawResults {
		operation, err := resources.CreateOrUpdateResult(ctx, r.Client, result)
		if err != nil {
			k8sgptReconcileErrorCount.Inc()
			return err

		}
		// Update metrics
		if operation == resources.CreatedResult {
			k8sgptNumberOfResultsByType.With(prometheus.Labels{
				"kind": result.Spec.Kind,
				"name": result.Name,
			}).Inc()
		} else if operation == resources.UpdatedResult {
			fmt.Printf("Updated successfully %s \n", result.Name)
		}

	}

	// We emit when result Status is not historical
	// and when user configures a sink for the first time
	latestResultList := &corev1alpha1.ResultList{}
	if err := r.List(ctx, latestResultList); err != nil {
		return err
	}
	if len(latestResultList.Items) == 0 {
		return r.setStatus(ctx, k8sgptConfig, k8sgptConfig.Status.State, "no result received")
	}
	sinkEnabled := k8sgptConfig.Spec.Sink != nil && k8sgptConfig.Spec.Sink.Type != "" && k8sgptConfig.Spec.Sink.Endpoint != ""

	var sinkType sinks.ISink
	if sinkEnabled {
		sinkType = sinks.NewSink(k8sgptConfig.Spec.Sink.Type)
		sinkType.Configure(*k8sgptConfig, *r.SinkClient)
	}

	for _, result := range latestResultList.Items {
		var res corev1alpha1.Result
		if err := r.Get(ctx, client.ObjectKey{Namespace: result.Namespace, Name: result.Name}, &res); err != nil {
			return err
		}

		if sinkEnabled {
			if res.Status.LifeCycle != resources.NoOpResult || res.Status.Webhook == "" {
				if err := sinkType.Emit(res.Spec); err != nil {
					k8sgptReconcileErrorCount.Inc()
					return err
				}
				res.Status.Webhook = k8sgptConfig.Spec.Sink.Endpoint
			}
		} else {
			// Remove the Webhook status from results
			res.Status.Webhook = ""
		}
		if err := r.Status().Update(ctx, &res); err != nil {
			k8sgptReconcileErrorCount.Inc()
			return err
		}
	}

	return r.setStatus(ctx, k8sgptConfig, corev1alpha1.StateReady, "result received")
}

// SetupWithManager sets up the controller with the Manager.
func (r *K8sGPTReconciler) SetupWithManager(mgr ctrl.Manager) error {
	predicates := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
	c := ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.K8sGPT{}).
		WithEventFilter(predicates).
		Complete(r)

	metrics.Registry.MustRegister(k8sgptReconcileErrorCount, k8sgptNumberOfResults, k8sgptNumberOfResultsByType)

	return c
}

func (r *K8sGPTReconciler) setStatus(ctx context.Context, k8sGPT *corev1alpha1.K8sGPT,
	newState corev1alpha1.State, message string,
) error {
	k8sGPT.Status.State = newState
	switch newState {
	case corev1alpha1.StateReady, corev1alpha1.StateWarning:
		k8sGPT.Status.SetInstallConditionStatus(metav1.ConditionTrue)
	case "":
		k8sGPT.Status.SetInstallConditionStatus(metav1.ConditionUnknown)
	case corev1alpha1.StateDeleting:
	case corev1alpha1.StateError:
	case corev1alpha1.StateProcessing:
	default:
		k8sGPT.Status.SetInstallConditionStatus(metav1.ConditionFalse)
	}
	k8sGPT.ManagedFields = nil
	k8sGPT.Status.LastOperation = corev1alpha1.LastOperation{
		Operation:      message,
		LastUpdateTime: metav1.NewTime(time.Now()),
	}
	if err := r.Status().Patch(ctx, k8sGPT, client.Apply,
		&client.SubResourcePatchOptions{PatchOptions: client.PatchOptions{FieldManager: fieldOwner}}); err != nil {
		return fmt.Errorf("error while updating status %s to: %w", newState, err)
	}

	return nil
}
