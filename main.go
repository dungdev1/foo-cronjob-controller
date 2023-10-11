package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	"github.com/dungdev1/foo-cronjob/pkg/controller"
	clientset "github.com/dungdev1/foo-cronjob/pkg/generated/clientset/versioned"
	informers "github.com/dungdev1/foo-cronjob/pkg/generated/informers/externalversions"
)

func main() {
	kubeconfig := ""
	curContext := "kz-uat"
	home, err := os.UserHomeDir()

	if err != nil {
		panic(err)
	}
	kubeconfig = filepath.Join(home, ".kube", "config")
	config, err := buildConfigFromFlagWithContext("", kubeconfig, curContext)
	if err != nil {
		panic(err.Error())
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	fooClientset, err := clientset.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	factory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	fooFactory := informers.NewSharedInformerFactory(fooClientset, 0)

	controller := controller.NewController(kubeClient, fooClientset, fooFactory.Foo().V1().CronJobs(), factory.Batch().V1().Jobs())
	ctx, cancel := context.WithCancel(context.Background())

	factory.Start(ctx.Done())
	fooFactory.Start(ctx.Done())

	go func() {
		err := controller.Run(ctx, 4)
		if err != nil {
			klog.Error(err)
			cancel()
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	shutdown := make(chan struct{}, 1)

	go func(done <-chan struct{}) {
		select {
		case <-done:
			shutdown <- struct{}{}
		case sig := <-sigs:
			klog.Infof("Received signal: %s, exiting...", sig)
			shutdown <- struct{}{}
		}
	}(ctx.Done())

	<-shutdown
	cancel()
	time.Sleep(1 * time.Second)
}

func buildConfigFromFlagWithContext(masterUrl, kubeconfigPath, context string) (*rest.Config, error) {
	if kubeconfigPath == "" && masterUrl == "" {
		klog.Warning("Neither --kubeconfig nor --master was specified.  Using the inClusterConfig.  This might not work.")
		kubeconfig, err := rest.InClusterConfig()
		if err == nil {
			return kubeconfig, nil
		}
		klog.Warning("error creating inClusterConfig, falling back to default config: ", err)
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterUrl}, CurrentContext: context},
	).ClientConfig()
}
