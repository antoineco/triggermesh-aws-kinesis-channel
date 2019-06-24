package main

import (
	"flag"
	"time"

	log "github.com/sirupsen/logrus"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	// Uncomment the following line to load the gcp plugin (only required to authenticate against GKE clusters).
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"github.com/triggermesh/aws-kinesis-provisioner/controller"
	clientset "github.com/triggermesh/aws-kinesis-provisioner/pkg/client/clientset/versioned"
	informers "github.com/triggermesh/aws-kinesis-provisioner/pkg/client/informers/externalversions"
	"github.com/triggermesh/aws-kinesis-provisioner/pkg/signals"
)

var (
	masterURL  string
	kubeconfig string
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
}

func main() {
	flag.Parse()

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	mainClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("Error building example clientset: %s", err.Error())
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	kinesissourceInformerFactory := informers.NewSharedInformerFactory(mainClient, time.Second*30)

	baseController := controller.NewController(kubeClient, mainClient,
		kubeInformerFactory.Apps().V1().Deployments(),
		kinesissourceInformerFactory.Kinesissource().V1().KinesisSources())

	// notice that there is no need to run Start methods in a separate goroutine. (i.e. go kubeInformerFactory.Start(stopCh)
	// Start method is non-blocking and runs all registered informers in a dedicated goroutine.
	kubeInformerFactory.Start(stopCh)
	kinesissourceInformerFactory.Start(stopCh)

	if err = baseController.Run(2, stopCh); err != nil {
		log.Fatalf("Error running controller: %s", err.Error())
	}
} 
