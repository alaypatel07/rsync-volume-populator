/*
Copyright 2022.

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
	"flag"
	"fmt"
	"os"

	rsyncv1alpha1 "github.com/alaypatel07/rsync-volume-populator/api/v1alpha1"
	rsyncvolumepopulator "github.com/alaypatel07/rsync-volume-populator/controllers"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	//+kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(rsyncv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func groupKindFromCRD(object runtime.Object) schema.GroupKind {
	return object.GetObjectKind().GroupVersionKind().GroupKind()
}

var version = rsyncv1alpha1.GroupVersion.Version

var prefix = rsyncv1alpha1.GroupVersion.Group

func main() {
	var (
		mode         string
		fileName     string
		fileContents string
		masterURL    string
		kubeconfig   string
		imageName    string
		showVersion  bool
		namespace    string
	)
	// Main arg
	flag.StringVar(&mode, "mode", "", "Mode to run in (controller, populate)")
	// Populate args
	flag.StringVar(&fileName, "file-name", "", "File name to populate")
	flag.StringVar(&fileContents, "file-contents", "", "Contents to populate file with")
	flag.StringVar(&namespace, "namespace", "", "Namespace for creating copy PVCs")
	// Controller args
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&imageName, "image-name", "", "Image to use for populating")
	flag.BoolVar(&showVersion, "version", false, "display the version string")
	flag.Parse()

	//opts := zap.Options{
	//	Development: true,
	//}
	//opts.BindFlags(flag.CommandLine)
	//
	//ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if showVersion {
		fmt.Println(os.Args[0], version)
		os.Exit(0)
	}

	switch mode {
	case "controller":
		var (
			groupName  = rsyncv1alpha1.GroupVersion.Group
			apiVersion = rsyncv1alpha1.GroupVersion.Version
			kind       = "Rsync"
			resource   = "rsyncs"
		)
		var (
			gk  = schema.GroupKind{Group: groupName, Kind: kind}
			gvr = schema.GroupVersionResource{Group: groupName, Version: apiVersion, Resource: resource}
		)
		err := rsyncvolumepopulator.RunController(masterURL, kubeconfig, imageName,
			namespace, prefix, gk, gvr)
		if err != nil {
			klog.Fatalf("error running controller", err, mode)
		}
	default:
		klog.Fatalf("Invalid mode: %s", mode)
	}
}
