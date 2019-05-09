// Copyright 2019 TriggerMesh, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"log"
	"os"

	eventingv1alpha1 "github.com/knative/eventing/pkg/apis/eventing/v1alpha1"
	"github.com/knative/eventing/pkg/provisioners"
	istiov1alpha3 "github.com/knative/pkg/apis/istio/v1alpha3"
	"github.com/knative/pkg/signals"
	"github.com/triggermesh/aws-kinesis-provisioner/pkg/controller/channel"
	"github.com/triggermesh/aws-kinesis-provisioner/pkg/controller/clusterchannelprovisioner"
	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// This is the main method for the IBM MQ Channel controller. It reconciles the
// ClusterChannelProvisioner itself and Channels that use the 'aws-kinesis' provisioner. It does not
// handle the anything at the data layer.
func main() {
	logConfig := provisioners.NewLoggingConfig()
	logger := provisioners.NewProvisionerLoggerFromConfig(logConfig)
	defer logger.Sync()
	logger = logger.With(
		zap.String("eventing.knative.dev/clusterChannelProvisioner", clusterchannelprovisioner.Name),
		zap.String("eventing.knative.dev/clusterChannelProvisionerComponent", "Controller"),
	)
	flag.Parse()

	mgr, err := manager.New(config.GetConfigOrDie(), manager.Options{})
	if err != nil {
		logger.Fatal("Error starting up.", zap.Error(err))
	}

	// Add custom types to this array to get them into the manager's scheme.
	eventingv1alpha1.AddToScheme(mgr.GetScheme())
	istiov1alpha3.AddToScheme(mgr.GetScheme())

	// The controllers for both the ClusterChannelProvisioner and the Channels created by that
	// ClusterChannelProvisioner run in this process.
	_, err = clusterchannelprovisioner.ProvideController(mgr, logger.Desugar())
	if err != nil {
		logger.Fatal("Unable to create Provisioner controller", zap.Error(err))
	}

	_, err = channel.ProvideController()(mgr, logger.Desugar())
	if err != nil {
		logger.Fatal("Unable to create Channel controller", zap.Error(err))
	}

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()
	// Start blocks forever.
	err = mgr.Start(stopCh)
	if err != nil {
		logger.Fatal("Manager.Start() returned an error", zap.Error(err))
	}
}

func getRequiredEnv(envKey string) string {
	val, defined := os.LookupEnv(envKey)
	if !defined {
		log.Fatalf("required environment variable not defined '%s'", envKey)
	}
	return val
}
