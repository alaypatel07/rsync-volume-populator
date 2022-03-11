/*
Copyright 2021 The Kubernetes Authors.

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
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	rsyncv1alpha1 "github.com/alaypatel07/rsync-volume-populator/api/v1alpha1"
	pipelinev1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	pipelineclient "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	pipelineinformers "github.com/tektoncd/pipeline/pkg/client/informers/externalversions"
	pipelinev1beta1lister "github.com/tektoncd/pipeline/pkg/client/listers/pipeline/v1beta1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/dynamiclister"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"knative.dev/pkg/apis"
)

const (
	populatorContainerName  = "populate"
	populatorPodPrefix      = "populate"
	populatorPodVolumeName  = "target"
	populatorPvcPrefix      = "prime"
	populatedFromAnnoSuffix = "populated-from"
	pvcFinalizerSuffix      = "populate-target-protection"
	annSelectedNode         = "volume.kubernetes.io/selected-node"
)

type empty struct{}

type stringSet struct {
	set map[string]empty
}

type controller struct {
	populatorNamespace string
	populatedFromAnno  string
	pvcFinalizer       string
	kubeClient         kubernetes.Interface
	imageName          string
	devicePath         string
	mountPath          string
	pvcLister          corelisters.PersistentVolumeClaimLister
	pvcSynced          cache.InformerSynced
	pvLister           corelisters.PersistentVolumeLister
	pvSynced           cache.InformerSynced
	podLister          corelisters.PodLister
	podSynced          cache.InformerSynced
	scLister           storagelisters.StorageClassLister
	scSynced           cache.InformerSynced
	unstLister         dynamiclister.Lister
	unstSynced         cache.InformerSynced
	pipelineRunLister  pipelinev1beta1lister.PipelineRunLister
	pipelineSynced     cache.InformerSynced
	mu                 sync.Mutex
	notifyMap          map[string]*stringSet
	cleanupMap         map[string]*stringSet
	workqueue          workqueue.RateLimitingInterface
	populatorArgs      func(bool, *unstructured.Unstructured) ([]string, error)
	gk                 schema.GroupKind
	pipelineClient     pipelineclient.Interface
}

func RunController(masterURL, kubeconfig, imageName, namespace, prefix string,
	gk schema.GroupKind, gvr schema.GroupVersionResource) error {
	klog.Infof("Starting populator controller for %s", gvr)

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stopCh)
		<-sigCh
		os.Exit(1) // second signal. Exit directly.
	}()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if nil != err {
		klog.Fatalf("Failed to create config: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if nil != err {
		klog.Fatalf("Failed to create client: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if nil != err {
		klog.Fatalf("Failed to create dynamic client: %v", err)
	}

	pipelineClientSet, err := pipelineclient.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("failed to create pipelineclient: %#v", err)
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, time.Second*30)
	pipelineInformerFactory := pipelineinformers.NewSharedInformerFactory(pipelineClientSet, time.Second*30)

	pvcInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()
	pvInformer := kubeInformerFactory.Core().V1().PersistentVolumes()
	podInformer := kubeInformerFactory.Core().V1().Pods()
	scInformer := kubeInformerFactory.Storage().V1().StorageClasses()
	unstInformer := dynInformerFactory.ForResource(gvr).Informer()
	pipelineRunInformer := pipelineInformerFactory.Tekton().V1beta1().PipelineRuns()

	c := &controller{
		kubeClient:         kubeClient,
		pipelineClient:     pipelineClientSet,
		imageName:          imageName,
		populatorNamespace: namespace,
		populatedFromAnno:  prefix + "/" + populatedFromAnnoSuffix,
		pvcFinalizer:       prefix + "/" + pvcFinalizerSuffix,
		pvcLister:          pvcInformer.Lister(),
		pvcSynced:          pvcInformer.Informer().HasSynced,
		pvLister:           pvInformer.Lister(),
		pvSynced:           pvInformer.Informer().HasSynced,
		podLister:          podInformer.Lister(),
		podSynced:          podInformer.Informer().HasSynced,
		scLister:           scInformer.Lister(),
		scSynced:           scInformer.Informer().HasSynced,
		pipelineRunLister:  pipelineRunInformer.Lister(),
		pipelineSynced:     pipelineRunInformer.Informer().HasSynced,
		unstLister:         dynamiclister.New(unstInformer.GetIndexer(), gvr),
		unstSynced:         unstInformer.HasSynced,
		notifyMap:          make(map[string]*stringSet),
		cleanupMap:         make(map[string]*stringSet),
		workqueue:          workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		gk:                 gk,
	}

	pvcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePVC,
		UpdateFunc: func(old, new interface{}) {
			newPvc := new.(*corev1.PersistentVolumeClaim)
			oldPvc := old.(*corev1.PersistentVolumeClaim)
			if newPvc.ResourceVersion == oldPvc.ResourceVersion {
				return
			}
			c.handlePVC(new)
		},
		DeleteFunc: c.handlePVC,
	})

	pvInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePV,
		UpdateFunc: func(old, new interface{}) {
			newPv := new.(*corev1.PersistentVolume)
			oldPv := old.(*corev1.PersistentVolume)
			if newPv.ResourceVersion == oldPv.ResourceVersion {
				return
			}
			c.handlePV(new)
		},
		DeleteFunc: c.handlePV,
	})

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePod,
		UpdateFunc: func(old, new interface{}) {
			newPod := new.(*corev1.Pod)
			oldPod := old.(*corev1.Pod)
			if newPod.ResourceVersion == oldPod.ResourceVersion {
				return
			}
			c.handlePod(new)
		},
		DeleteFunc: c.handlePod,
	})

	scInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleSC,
		UpdateFunc: func(old, new interface{}) {
			newSc := new.(*storagev1.StorageClass)
			oldSc := old.(*storagev1.StorageClass)
			if newSc.ResourceVersion == oldSc.ResourceVersion {
				return
			}
			c.handleSC(new)
		},
		DeleteFunc: c.handleSC,
	})

	unstInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleUnstructured,
		UpdateFunc: func(old, new interface{}) {
			newUnstructured := new.(*unstructured.Unstructured)
			oldUnstructured := old.(*unstructured.Unstructured)
			if newUnstructured.GetResourceVersion() == oldUnstructured.GetResourceVersion() {
				return
			}
			c.handleUnstructured(new)
		},
		DeleteFunc: c.handleUnstructured,
	})

	pipelineRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePipelineRun,
		UpdateFunc: func(old, new interface{}) {
			newSc := new.(*pipelinev1beta1.PipelineRun)
			oldSc := old.(*pipelinev1beta1.PipelineRun)
			if newSc.ResourceVersion == oldSc.ResourceVersion {
				return
			}
			c.handlePipelineRun(new)
		},
		DeleteFunc: c.handlePipelineRun,
	})

	kubeInformerFactory.Start(stopCh)
	dynInformerFactory.Start(stopCh)
	pipelineInformerFactory.Start(stopCh)

	if err = c.run(stopCh); nil != err {
		klog.Fatalf("Failed to run controller: %v", err)
	}
	return err
}

func (c *controller) addNotification(keyToCall, objType, namespace, name string) {
	var key string
	if 0 == len(namespace) {
		key = objType + "/" + name
	} else {
		key = objType + "/" + namespace + "/" + name
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.notifyMap[key]
	if nil == s {
		s = &stringSet{make(map[string]empty)}
		c.notifyMap[key] = s
	}
	s.set[keyToCall] = empty{}
	s = c.cleanupMap[keyToCall]
	if nil == s {
		s = &stringSet{make(map[string]empty)}
		c.cleanupMap[keyToCall] = s
	}
	s.set[key] = empty{}
}

func (c *controller) cleanupNofications(keyToCall string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.cleanupMap[keyToCall]
	if nil == s {
		return
	}
	for key := range s.set {
		t := c.notifyMap[key]
		if nil == t {
			continue
		}
		delete(t.set, keyToCall)
		if 0 == len(t.set) {
			delete(c.notifyMap, key)
		}
	}
}

func translateObject(obj interface{}) metav1.Object {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return nil
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return nil
		}
	}
	return object
}

func (c *controller) handleMapped(obj interface{}, objType string) {
	object := translateObject(obj)
	if nil == object {
		return
	}
	var key string
	if 0 == len(object.GetNamespace()) {
		key = objType + "/" + object.GetName()
	} else {
		key = objType + "/" + object.GetNamespace() + "/" + object.GetName()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.notifyMap[key]; ok {
		for k := range s.set {
			c.workqueue.Add(k)
		}
	}
}

func (c *controller) handlePVC(obj interface{}) {
	c.handleMapped(obj, "pvc")
	object := translateObject(obj)
	if nil == object {
		return
	}
	if c.populatorNamespace != object.GetNamespace() {
		c.workqueue.Add("pvc/" + object.GetNamespace() + "/" + object.GetName())
	}
}

func (c *controller) handlePV(obj interface{}) {
	c.handleMapped(obj, "pv")
}

func (c *controller) handlePod(obj interface{}) {
	c.handleMapped(obj, "pod")
}

func (c *controller) handleSC(obj interface{}) {
	c.handleMapped(obj, "sc")
}

func (c *controller) handlePipelineRun(obj interface{}) {
	c.handleMapped(obj, "pipelinerun")
}

func (c *controller) handleUnstructured(obj interface{}) {
	c.handleMapped(obj, "unstructured")
}

func (c *controller) run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	ok := cache.WaitForCacheSync(stopCh, c.pvcSynced, c.pvSynced, c.podSynced, c.scSynced, c.unstSynced)
	if !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh

	return nil
}

func (c *controller) runWorker() {
	processNextWorkItem := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		var err error
		parts := strings.Split(key, "/")
		fmt.Println(key)
		switch parts[0] {
		case "pvc":
			if 3 != len(parts) {
				utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
				return nil
			}
			err = c.syncPvc(context.TODO(), key, parts[1], parts[2])
		default:
			utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
			return nil
		}
		if nil != err {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		return nil
	}

	for {
		obj, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}
		err := processNextWorkItem(obj)
		if nil != err {
			utilruntime.HandleError(err)
		}
	}
}

func (c *controller) syncPvc(ctx context.Context, key, pvcNamespace, pvcName string) error {
	if c.populatorNamespace == pvcNamespace {
		// Ignore PVCs in our own working namespace
		klog.V(2).Info("ignore pvc, because in populator namespace", pvcName, c.populatorNamespace)
		return nil
	}

	var err error
	var pvc *corev1.PersistentVolumeClaim
	pvc, err = c.pvcLister.PersistentVolumeClaims(pvcNamespace).Get(pvcName)
	if nil != err {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("pvc '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	dataSourceRef := pvc.Spec.DataSourceRef
	if nil == dataSourceRef {
		// Ignore PVCs without a datasource
		return nil
	}

	if c.gk.Group != *dataSourceRef.APIGroup || c.gk.Kind != dataSourceRef.Kind || "" == dataSourceRef.Name {
		// Ignore PVCs that aren't for this populator to handle
		return nil
	}

	u, err := c.unstLister.Namespace(pvc.Namespace).Get(dataSourceRef.Name)
	if nil != err {
		if !errors.IsNotFound(err) {
			return err
		}
		klog.Error(err, "unable to get unstructured object", "namespace", pvc.Namespace, "name", dataSourceRef.Name)
		c.addNotification(key, "unstructured", pvc.Namespace, dataSourceRef.Name)
		// We'll get called again later when the data source exists
		return nil
	}

	rsync, err := rsyncCRFromUnstructured(u)
	if err != nil {
		c.addNotification(key, "unstructured", pvc.Namespace, dataSourceRef.Name)
		klog.V(2).ErrorS(err, "unable to type cast rsync CR", "namespace", pvc.Namespace, "name", pvc.Name)
		return err
	}

	var waitForFirstConsumer bool
	var nodeName string
	if nil != pvc.Spec.StorageClassName {
		storageClassName := *pvc.Spec.StorageClassName

		var storageClass *storagev1.StorageClass
		storageClass, err = c.scLister.Get(storageClassName)
		if nil != err {
			if !errors.IsNotFound(err) {
				return err
			}
			c.addNotification(key, "sc", "", storageClassName)
			// We'll get called again later when the storage class exists
			return nil
		}

		if nil != storageClass.VolumeBindingMode && storagev1.VolumeBindingWaitForFirstConsumer == *storageClass.VolumeBindingMode {
			waitForFirstConsumer = true
			nodeName = pvc.Annotations[annSelectedNode]
			if "" == nodeName {
				// Wait for the PVC to get a node name before continuing
				return nil
			}
		}
	}

	// Look for the populator pipeline
	pipelineRunName := fmt.Sprintf("%s-%s", populatorPodPrefix, pvc.UID)
	// TODO: add informer in pipelinrun to handle these events
	c.addNotification(key, "pipelinerun", c.populatorNamespace, pipelineRunName)
	pipelineRun, err := c.pipelineRunLister.PipelineRuns(c.populatorNamespace).Get(pipelineRunName)
	if err != nil {
		if !errors.IsNotFound(err) {
			klog.ErrorS(err, "unable to get the pipelinerun", "name", pipelineRunName, "namespace", c.populatorNamespace)
			return err
		}
	}

	// Look for PVC'
	// TODO: change pvc names to populatePrefix-pvcUID once pvc rename name is supported in crane
	pvcPrimeName := fmt.Sprintf("%s", pvc.Name)
	c.addNotification(key, "pvc", c.populatorNamespace, pvcPrimeName)
	pvcPrime, err := c.pvcLister.PersistentVolumeClaims(c.populatorNamespace).Get(pvcPrimeName)
	if err != nil {
		if !errors.IsNotFound(err) {
			klog.ErrorS(err, "unable to get pvcPrime", "name", pvcPrimeName, "namespace", c.populatorNamespace)
			return err
		}
	}

	pvc = pvc.DeepCopy()
	//Start creating the pipelines
	klog.InfoS("pvc statuses", "pvc", pvc.Name, "namespace", pvc.Namespace, "phase", pvc.Status.Phase)
	if pvcPrime != nil {
		klog.InfoS("pvc prime statuses", "pvc prime", pvcPrime.Name, "namespace", pvcPrime.Namespace, "phase", pvcPrime.Status.Phase)
	}
	switch {
	case pvc.Status.Phase == corev1.ClaimBound && pvcPrime == nil:
		klog.V(4).Info("pvc successfully reconciled", "name", pvc.Name, "namespace", pvc.Namespace)
		return nil
	case pvc.Status.Phase == corev1.ClaimBound && pvcPrime != nil && pvcPrime.Status.Phase == corev1.ClaimLost:
		klog.V(2).Info("pvc claim bound, garbage collecting")
		err := c.garbageCollect(ctx, key, pvc, pvcPrime, pipelineRun)
		if err != nil {
			klog.ErrorS(err, "error garbage collecting resources in populator namespace", "name", pvc.Name, "namespace", pvc.Namespace)
			return err
		}
		// requeue one more time just to be safe
		c.addNotification(key, "pvc", pvc.Namespace, pvc.Name)
		return nil
	case pvc.Status.Phase == corev1.ClaimPending:
		klog.InfoS("attempting to populate pvc", "name", pvc.Name, "namespace", pvc.Namespace)
	}
	// Ensure the PVC has a finalizer on it so we can clean up the stuff we create
	err = c.ensureFinalizer(ctx, pvc, c.pvcFinalizer, true)
	if nil != err {
		return err
	}

	if pvcPrime == nil {
		pvcPrime = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcPrimeName,
				Namespace: c.populatorNamespace,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:        pvc.Spec.Resources,
				StorageClassName: pvc.Spec.StorageClassName,
				VolumeMode:       pvc.Spec.VolumeMode,
			},
		}
		if waitForFirstConsumer {
			pvcPrime.Annotations = map[string]string{
				annSelectedNode: nodeName,
			}
		}
		c.addNotification(key, "pvc", c.populatorNamespace, pvcPrimeName)
		_, err = c.kubeClient.CoreV1().PersistentVolumeClaims(c.populatorNamespace).Create(ctx, pvcPrime, metav1.CreateOptions{})
		if nil != err {
			return err
		}
	}

	if pipelineRun == nil {
		p := generatePipelineRun(pipelineRunName, c.populatorNamespace, pvcPrimeName, rsync.Spec.SourcePVC.Namespace)
		c.addNotification(key, "pipelinerun", c.populatorNamespace, pipelineRunName)
		pipelineRun, err = c.pipelineClient.TektonV1beta1().PipelineRuns(c.populatorNamespace).Create(ctx, p, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}

	if pipelineRun.Status.Conditions == nil {
		return fmt.Errorf("pipeline run condition is nil")
	}

	if pipelineRun.Status.Conditions != nil {
		for _, cond := range pipelineRun.Status.Conditions {
			if cond.Type == apis.ConditionSucceeded {
				switch cond.Status {
				case corev1.ConditionTrue:
					// succeeded
					break
				case corev1.ConditionFalse:
					// failed
					klog.Warning("pipeline run has succeeded condition false", pipelineRunName)
					return fmt.Errorf("pipeline run has succeeded false condition")
				case corev1.ConditionUnknown:
					c.addNotification(key, "pipelinerun", c.populatorNamespace, pipelineRunName)
					return fmt.Errorf("pipline run condition unknown, syncing")
				}
			}
		}
	}

	// Get PV
	var pv *corev1.PersistentVolume
	c.addNotification(key, "pv", "", pvcPrime.Spec.VolumeName)
	pv, err = c.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvcPrime.Spec.VolumeName, metav1.GetOptions{})
	if nil != err {
		if !errors.IsNotFound(err) {
			return err
		}
		// We'll get called again later when the PV exists
		return nil
	}

	// Examine the claimref for the PV and see if it's bound to the correct PVC
	claimRef := pv.Spec.ClaimRef
	if claimRef.Name != pvc.Name || claimRef.Namespace != pvc.Namespace || claimRef.UID != pvc.UID {
		// Make new PV with strategic patch values to perform the PV rebind
		patchPv := corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pv.Name,
				Annotations: map[string]string{},
			},
			Spec: corev1.PersistentVolumeSpec{
				ClaimRef: &corev1.ObjectReference{
					Namespace:       pvc.Namespace,
					Name:            pvc.Name,
					UID:             pvc.UID,
					ResourceVersion: pvc.ResourceVersion,
				},
			},
		}
		patchPv.Annotations[c.populatedFromAnno] = pvc.Namespace + "/" + dataSourceRef.Name
		var patchData []byte
		patchData, err = json.Marshal(patchPv)
		if nil != err {
			return err
		}
		_, err = c.kubeClient.CoreV1().PersistentVolumes().Patch(ctx, pv.Name, types.StrategicMergePatchType,
			patchData, metav1.PatchOptions{})
		if nil != err {
			return err
		}

		// Don't start cleaning up yet -- we need to bind controller to acknowledge
		// the switch
		return nil
	}

	// Wait for the bind controller to rebind the PV
	c.addNotification(key, "pvc", pvc.Namespace, pvc.Name)

	return nil
}

func rsyncCRFromUnstructured(u *unstructured.Unstructured) (*rsyncv1alpha1.Rsync, error) {
	rsync := &rsyncv1alpha1.Rsync{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), rsync)
	if nil != err {
		return nil, err
	}
	return rsync, nil
}
func (c *controller) ensureFinalizer(ctx context.Context, pvc *corev1.PersistentVolumeClaim, finalizer string, want bool) error {
	finalizers := pvc.GetFinalizers()
	found := false
	foundIdx := -1
	for i, v := range finalizers {
		if finalizer == v {
			found = true
			foundIdx = i
			break
		}
	}
	if found == want {
		// Nothing to do in this case
		return nil
	}

	type patchOp struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value,omitempty"`
	}

	var patch []patchOp

	if want {
		// Add the finalizer to the end of the list
		patch = []patchOp{
			{
				Op:    "test",
				Path:  "/metadata/finalizers",
				Value: finalizers,
			},
			{
				Op:    "add",
				Path:  "/metadata/finalizers/-",
				Value: finalizer,
			},
		}
	} else {
		// Remove the finalizer from the list index where it was found
		path := fmt.Sprintf("/metadata/finalizers/%d", foundIdx)
		patch = []patchOp{
			{
				Op:    "test",
				Path:  path,
				Value: finalizer,
			},
			{
				Op:   "remove",
				Path: path,
			},
		}
	}

	data, err := json.Marshal(patch)
	if nil != err {
		return err
	}
	_, err = c.kubeClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Patch(ctx, pvc.Name, types.JSONPatchType,
		data, metav1.PatchOptions{})
	if nil != err {
		return err
	}

	return nil
}

func (c *controller) garbageCollect(ctx context.Context, key string, pvc, pvcPrime *corev1.PersistentVolumeClaim, pipelineRun *pipelinev1beta1.PipelineRun) error {
	var err error
	if pipelineRun != nil {
		err = c.pipelineClient.TektonV1alpha1().PipelineRuns(c.populatorNamespace).Delete(ctx, pipelineRun.Name, metav1.DeleteOptions{})
		if err != nil {
			klog.ErrorS(err, "error deleting pipeline run", "name", pipelineRun.Name, "namespace", c.populatorNamespace)
			return err
		}
	}

	if pvcPrime != nil {
		err = c.kubeClient.CoreV1().PersistentVolumeClaims(c.populatorNamespace).Delete(ctx, pvcPrime.Name, metav1.DeleteOptions{})
		if nil != err {
			klog.ErrorS(err, "error deleting pvc prime", "name", pvcPrime.Name, "namespace", c.populatorNamespace)
			return err
		}
	}

	// Make sure the PVC finalizer is gone
	err = c.ensureFinalizer(ctx, pvc, c.pvcFinalizer, false)
	if nil != err {
		klog.ErrorS(err, "error removing finalizer on pvc", "name", pvc.Name, "namespace", pvc.Namespace)
		return err
	}

	// Clean up our internal callback maps
	c.cleanupNofications(key)
	return nil
}
