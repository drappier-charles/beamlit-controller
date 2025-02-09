/*
Copyright 2024.

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
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"

	beamlitauthorizationv1alpha1 "github.com/beamlit/beamlit-controller/api/v1alpha1/authorization"
	beamlitdeploymentv1alpha1 "github.com/beamlit/beamlit-controller/api/v1alpha1/deployment"
	beamlitclientset "github.com/beamlit/beamlit-controller/gateway/clientset"
	"github.com/beamlit/beamlit-controller/internal/beamlit"
	"github.com/beamlit/beamlit-controller/internal/config"
	"github.com/beamlit/beamlit-controller/internal/controller"
	"github.com/beamlit/beamlit-controller/internal/dataplane/configurer"
	"github.com/beamlit/beamlit-controller/internal/dataplane/offloader"
	"github.com/beamlit/beamlit-controller/internal/informers/health"
	"github.com/beamlit/beamlit-controller/internal/informers/metric"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(beamlitauthorizationv1alpha1.AddToScheme(scheme))
	utilruntime.Must(beamlitdeploymentv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

//nolint:gocyclo
func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "Path to the config file")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := &config.Config{}
	cfg.Default()
	cfgFile, err := os.OpenFile(cfgPath, os.O_RDONLY, 0)
	if err != nil {
		setupLog.Error(err, "unable to open config file")
		os.Exit(1)
	}
	defer func() {
		if err := cfgFile.Close(); err != nil {
			setupLog.Error(err, "unable to close config file")
			os.Exit(1)
		}
	}()
	if err := cfg.FromFile(cfgPath, cfgFile); err != nil {
		setupLog.Error(err, "unable to load config file")
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		setupLog.Error(err, "invalid config")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !*cfg.EnableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	ctrlOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   *cfg.MetricsAddr,
			SecureServing: *cfg.SecureMetrics,
			TLSOpts:       tlsOpts,
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: *cfg.ProbeAddr,
		LeaderElection:         *cfg.EnableLeaderElection,
		LeaderElectionID:       "0e22b10b.beamlit.com",
	}

	namespacesList := make(map[string]cache.Config)
	if *cfg.Namespaces != "" {
		for _, ns := range strings.Split(*cfg.Namespaces, ",") {
			namespacesList[ns] = cache.Config{}
		}
	}

	if len(namespacesList) > 0 {
		ctrlOpts.NewCache = cache.New
		ctrlOpts.Cache = cache.Options{
			DefaultNamespaces: namespacesList,
		}
	}

	ctx := ctrl.SetupSignalHandler()

	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to get config")
		os.Exit(1)
	}
	mgr, err := ctrl.NewManager(kubeConfig, ctrlOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	metricInformer, err := metric.NewMetricInformer(ctx, kubeConfig, metric.K8SMetricInformerType)
	if err != nil {
		setupLog.Error(err, "unable to create metrics watcher")
		os.Exit(1)
	}

	if cfg.MetricInformerConfig != nil {
		if cfg.MetricInformerConfig.Type == config.MetricInformerTypePrometheus {
			restConfig := &rest.Config{
				Host: cfg.MetricInformerConfig.Prometheus.Address,
			}
			metricInformer, err = metric.NewMetricInformer(ctx, restConfig, metric.PrometheusMetricInformerType)
			if err != nil {
				setupLog.Error(err, "unable to create prometheus metrics watcher")
				os.Exit(1)
			}

		}
	}

	metricChan := metricInformer.Start(ctx)

	healthInformer, err := health.NewHealthInformer(ctx, kubeConfig, health.K8SHealthInformerType)
	if err != nil {
		setupLog.Error(err, "unable to create health watcher")
		os.Exit(1)
	}
	healthChan := healthInformer.Start(ctx)

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		setupLog.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	beamlitClient, err := beamlit.NewClient()
	if err != nil {
		setupLog.Error(err, "unable to create beamlit client")
		os.Exit(1)
	}

	configurer, err := configurer.NewConfigurer(ctx, configurer.KubernetesConfigurerType, clientset)
	if err != nil {
		setupLog.Error(err, "unable to create configurer")
		os.Exit(1)
	}

	go func() {
		if err := configurer.Start(ctx, &beamlitdeploymentv1alpha1.ServiceReference{
			ObjectReference: corev1.ObjectReference{
				Namespace: *cfg.ProxyService.Namespace,
				Name:      *cfg.ProxyService.Name,
			},
			TargetPort: int32(*cfg.ProxyService.Port),
		}); err != nil {
			setupLog.Error(err, "unable to start configurer")
		}
	}()

	offloader, err := offloader.NewOffloader(
		ctx,
		offloader.BeamlitGatewayOffloaderType,
		clientset,
		beamlitclientset.NewClientSet(
			http.DefaultClient,
			fmt.Sprintf(
				"%s.%s.svc.cluster.local:%d",
				*cfg.ProxyService.Name,
				*cfg.ProxyService.Namespace,
				*cfg.ProxyService.AdminPort,
			),
		),
	)
	if err != nil {
		setupLog.Error(err, "unable to create offloader")
		os.Exit(1)
	}

	client := mgr.GetClient()
	scheme := mgr.GetScheme()
	ctrl := &controller.ModelDeploymentReconciler{
		Client:               client,
		Scheme:               scheme,
		BeamlitClient:        beamlitClient,
		MetricInformer:       metricInformer,
		MetricStatusChan:     metricChan,
		Configurer:           configurer,
		HealthInformer:       healthInformer,
		HealthStatusChan:     healthChan,
		Offloader:            offloader,
		ManagedModels:        make(map[string]controller.ManagedModel),
		OngoingOffloadings:   sync.Map{},
		ModelState:           sync.Map{},
		DefaultRemoteBackend: nil,
		BeamlitModels:        make(map[string]string),
	}

	if cfg.DefaultRemoteBackend.Host != nil {
		ctrl.DefaultRemoteBackend = &beamlitdeploymentv1alpha1.RemoteBackend{
			Host: *cfg.DefaultRemoteBackend.Host,
		}
		if cfg.DefaultRemoteBackend.AuthConfig != nil {
			ctrl.DefaultRemoteBackend.AuthConfig = cfg.DefaultRemoteBackend.AuthConfig
		}
		if cfg.DefaultRemoteBackend.Scheme != nil {
			ctrl.DefaultRemoteBackend.Scheme = *cfg.DefaultRemoteBackend.Scheme
		}
		if cfg.DefaultRemoteBackend.PathPrefix != nil {
			ctrl.DefaultRemoteBackend.PathPrefix = *cfg.DefaultRemoteBackend.PathPrefix
		}
		if cfg.DefaultRemoteBackend.HeadersToAdd != nil {
			ctrl.DefaultRemoteBackend.HeadersToAdd = cfg.DefaultRemoteBackend.HeadersToAdd
		}
	}

	if err = ctrl.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelDeployment")
		os.Exit(1)
	}
	if err = (&controller.PolicyReconciler{
		Client:          client,
		Scheme:          scheme,
		BeamlitClient:   beamlitClient,
		ManagedPolicies: make(map[string]controller.ManagedPolicyRef),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Policy")
		os.Exit(1)
	}
	if err = (&controller.ToolDeploymentReconciler{
		Client: client,
		Scheme: scheme,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ToolDeployment")
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

	go func() {
		if err := ctrl.WatchForInformerUpdates(ctx); err != nil {
			setupLog.Error(err, "unable to watch for informer updates")
		}
	}()

	go func() {
		if err := ctrl.PolicyUpdate(ctx); err != nil {
			setupLog.Error(err, "unable to update policies")
		}
	}()

	go setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
