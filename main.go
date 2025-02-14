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

package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/client-go/util/workqueue"
	controllerRuntime "sigs.k8s.io/controller-runtime/pkg/controller"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1alpha1 "github.com/k8sgpt-ai/k8sgpt-operator/api/v1alpha1"
	"github.com/k8sgpt-ai/k8sgpt-operator/controllers"
	"github.com/k8sgpt-ai/k8sgpt-operator/pkg/integrations"
	"github.com/k8sgpt-ai/k8sgpt-operator/pkg/sinks"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const operatorName = "ai-sre"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(corev1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var tokenExpireTime time.Duration
	var failureBaseDelay, failureMaxDelay time.Duration
	var rateLimiterBurst, rateLimiterFrequency int
	flag.DurationVar(&tokenExpireTime, "token-expire-time", 11*time.Hour, "token expire time")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.IntVar(&rateLimiterBurst, "rate-limiter-burst", 200,
		"Indicates the rateLimiterBurstDefault value for the bucket rate limiter.")
	flag.IntVar(&rateLimiterFrequency, "rate-limiter-frequency", 30,
		"Indicates the bucket rate limiter frequency, signifying no. of events per second.")
	flag.DurationVar(&failureBaseDelay, "failure-base-delay", 100*time.Millisecond,
		"Indicates the failure base delay in seconds for rate limiter.")
	flag.DurationVar(&failureMaxDelay, "failure-max-delay", 10*time.Second,
		"Indicates the failure max delay in seconds")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if os.Getenv("LOCAL_MODE") != "" {
		setupLog.Info("Running in local mode")
		min := 7000
		max := 8000
		metricsAddr = fmt.Sprintf(":%d", rand.Intn(max-min+1)+min)
		probeAddr = fmt.Sprintf(":%d", rand.Intn(max-min+1)+min)
		setupLog.Info(fmt.Sprintf("Metrics address: %s", metricsAddr))
		setupLog.Info(fmt.Sprintf("Probe address: %s", probeAddr))
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ea9c19f7.k8sgpt.ai",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	integration, err := integrations.NewIntegrations(mgr.GetClient(), context.Background())
	if err != nil {
		setupLog.Error(err, "unable to create REST client to initialise Integrations")
		os.Exit(1)
	}

	timeout, exists := os.LookupEnv("OPERATOR_SINK_WEBHOOK_TIMEOUT_SECONDS")
	if !exists {
		timeout = "35s"
	}

	sinkTimeout, err := time.ParseDuration(timeout)
	if err != nil {
		setupLog.Error(err, "unable to read webhook timeout value")
		os.Exit(1)
	}
	sinkClient := sinks.NewClient(sinkTimeout)

	options := controllerRuntime.Options{}
	options.RateLimiter = workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(failureBaseDelay, failureMaxDelay),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(rateLimiterFrequency), rateLimiterBurst)},
	)

	if err = (&controllers.K8sGPTReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		TokenExpireTime: tokenExpireTime,
		Integrations:    integration,
		SinkClient:      sinkClient,
		EventRecorder:   mgr.GetEventRecorderFor(operatorName),
	}).SetupWithManager(mgr, options); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "K8sGPT")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
