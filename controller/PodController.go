package controller

import (
	"fmt"

	log "github.com/Sirupsen/logrus"
	shared "github.com/dgkanatsios/azuregameserversscalingkubernetes/shared"
	corev1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	listercorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	record "k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const podControllerAgentName = "pod-controller"

type PodController struct {
	podClient       typedcorev1.PodsGetter
	podLister       listercorev1.PodLister
	podListerSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

func NewPodController(client *kubernetes.Clientset, podInformer informercorev1.PodInformer) *PodController {
	// Create event broadcaster
	log.Info("Creating event broadcaster for Pod controller")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(log.Printf)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: client.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: podControllerAgentName})

	c := &PodController{
		podClient:       client.CoreV1(),
		podLister:       podInformer.Lister(),
		podListerSynced: podInformer.Informer().HasSynced,
		workqueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "PodSync"),
		recorder:        recorder,
	}

	log.Info("Setting up event handlers for Pod Controller")

	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {

				c.handlePod(obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				newPod := newObj.(*corev1.Pod)
				oldPod := oldObj.(*corev1.Pod)
				if newPod.ResourceVersion == oldPod.ResourceVersion {
					// Periodic resync will send update events for all known Pods.
					// Two different versions of the same Pod will always have different RVs.
					return
				}
				c.handlePod(newObj)
			},
			DeleteFunc: func(obj interface{}) {
				c.handlePod(obj)
			},
		},
	)

	return c
}

// RunWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *PodController) RunWorker() {
	// hot loop until we're told to stop.  processNextWorkItem will
	// automatically wait until there's work available, so we don't worry
	// about secondary waits
	log.Info("Starting loop for Pod controller")
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false
// when it's time to quit.
func (c *PodController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// DedicatedGameServer resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		log.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

func podBelongsToDedicatedGameServer(pod *corev1.Pod) bool {
	podLabels := pod.ObjectMeta.Labels

	for labelKey := range podLabels {
		if labelKey == "DedicatedGameServer" {
			return true
		}
	}
	return false
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Pod resource
// with the current status of the resource.
func (c *PodController) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)

	if namespace == "kube-system" {
		msg := fmt.Sprintf("Skipping kube-system pod %s", name)
		log.Print(msg)
		return nil
	}

	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// try to get the Pod
	pod, err := c.podLister.Pods(namespace).Get(name)

	if err != nil {
		if errors.IsNotFound(err) {
			// Pod not found
			runtime.HandleError(fmt.Errorf("Pod '%s' in work queue no longer exists", key))
			// delete it from table storage
			shared.DeleteEntity(namespace, name)
			return nil
		}
		log.Print(err.Error())
		return err
	}

	// pod exists, so let's update status if it has changed
	shared.UpsertEntity(&shared.StorageEntity{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Status:    string(pod.Status.Phase),
	})

	c.recorder.Event(pod, corev1.EventTypeNormal, shared.SuccessSynced, fmt.Sprintf(shared.MessageResourceSynced, "Pod", pod.Name))
	return nil
}

func (c *PodController) handlePod(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding Pod object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding Pod object tombstone, invalid type"))
			return
		}
		log.Infof("Recovered deleted Pod object '%s' from tombstone", object.GetName())
	}

	pod := obj.(*corev1.Pod)
	if !podBelongsToDedicatedGameServer(pod) {
		log.Printf("Ignoring non-DedicatedGameServer pod %s", pod.Name)
		return
	}

	c.enqueuePod(pod)
}

// enqueuePod takes a Pod resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Pod.
func (c *PodController) enqueuePod(obj interface{}) {

	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

func (c *PodController) Workqueue() workqueue.RateLimitingInterface {
	return c.workqueue
}

func (c *PodController) ListersSynced() []cache.InformerSynced {
	return []cache.InformerSynced{c.podListerSynced}
}