/*
Copyright 2018 The Kubernetes Authors.

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

package dynamic

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog"

	auditregv1alpha1 "k8s.io/api/auditregistration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditinstall "k8s.io/apiserver/pkg/apis/audit/install"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/apiserver/pkg/audit"
	webhook "k8s.io/apiserver/pkg/util/webhook"
	bufferedplugin "k8s.io/apiserver/plugin/pkg/audit/buffered"
	auditinformer "k8s.io/client-go/informers/auditregistration/v1alpha1"
	auditlisters "k8s.io/client-go/listers/auditregistration/v1alpha1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// PluginName is the name reported in error metrics.
const PluginName = "dynamic"

// Config holds the configuration for the dynamic backend
type Config struct {
	// AuditSinkInformer is informer for AuditSink resources
	AuditSinkInformer auditinformer.AuditSinkInformer
	// AuditClassInformer is informer for AuditClass resources
	AuditClassInformer auditinformer.AuditClassInformer
	// WorkersCount is the number of workque workers keeping AuditSynk delegates in sync with AuditSink and AudtiClass resources
	WorkersCount int
	// EventConfig holds the configuration for event notifications about the AuditSink API objects
	EventConfig EventConfig
	// BufferedConfig is the runtime buffered configuration
	BufferedConfig *bufferedplugin.BatchConfig
	// WebhookConfig holds the configuration for outgoing webhooks
	WebhookConfig WebhookConfig
}

// WebhookConfig holds the configurations for outgoing webhooks
type WebhookConfig struct {
	// AuthInfoResolverWrapper provides the webhook authentication for in-cluster endpoints
	AuthInfoResolverWrapper webhook.AuthenticationInfoResolverWrapper
	// ServiceResolver knows how to convert a webhook service reference into an actual location.
	ServiceResolver webhook.ServiceResolver
}

// EventConfig holds the configurations for sending event notifiations about AuditSink API objects
type EventConfig struct {
	// Sink for emitting events
	Sink record.EventSink
	// Source holds the source information about the event emitter
	Source corev1.EventSource
}

// delegate represents a delegate backend that was created from an audit sink configuration
type delegate struct {
	audit.Backend
	configuration *auditregv1alpha1.AuditSink
	stopChan      chan struct{}
}

// gracefulShutdown will gracefully shutdown the delegate
func (d *delegate) gracefulShutdown() {
	close(d.stopChan)
	d.Shutdown()
}

// NewBackend returns a backend that dynamically updates its configuration
// based on a shared informer.
func NewBackend(c *Config) (audit.Backend, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(c.EventConfig.Sink)

	scheme := runtime.NewScheme()
	err := auditregv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	recorder := eventBroadcaster.NewRecorder(scheme, c.EventConfig.Source)

	if c.BufferedConfig == nil {
		c.BufferedConfig = NewDefaultWebhookBatchConfig()
	}
	cm, err := webhook.NewClientManager(auditv1.SchemeGroupVersion, func(s *runtime.Scheme) error {
		auditinstall.Install(s)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// TODO: need a way of injecting authentication before beta
	authInfoResolver, err := webhook.NewDefaultAuthenticationInfoResolver("")
	if err != nil {
		return nil, err
	}
	cm.SetAuthenticationInfoResolver(authInfoResolver)
	cm.SetServiceResolver(c.WebhookConfig.ServiceResolver)
	cm.SetAuthenticationInfoResolverWrapper(c.WebhookConfig.AuthInfoResolverWrapper)

	b := &backend{
		config:               c,
		delegates:            atomic.Value{},
		delegateUpdateMutex:  sync.Mutex{},
		webhookClientManager: cm,
		recorder:             recorder,
	}

	b.auditSinkControler = newAuditSinkControler(c.AuditSinkInformer, c.AuditClassInformer, c.WorkersCount, b)

	b.delegates.Store(syncedDelegates{})

	return b, nil
}

type backend struct {
	// delegateUpdateMutex holds an update lock on the delegates
	delegateUpdateMutex  sync.Mutex
	config               *Config
	delegates            atomic.Value
	webhookClientManager webhook.ClientManager
	recorder             record.EventRecorder
	auditSinkControler   runner
}

// delegateManager defines the operations applicable to delegates that
// are exposed to the AuditSink controller.
type delegateManager interface {
	registerAndRunNewDelegate(sink *auditregv1alpha1.AuditSink) error
	unregisterAndStopDelegate(uid types.UID)
}

type syncedDelegates map[types.UID]*delegate

// Names returns the names of the delegate configurations
func (s syncedDelegates) Names() []string {
	names := []string{}
	for _, delegate := range s {
		names = append(names, delegate.configuration.Name)
	}
	return names
}

// ProcessEvents proccesses the given events per current delegate map
func (b *backend) ProcessEvents(events ...*auditinternal.Event) bool {
	for _, d := range b.GetDelegates() {
		d.ProcessEvents(events...)
	}
	// Returning true regardless of results, since dynamic audit backends
	// can never cause apiserver request to fail.
	return true
}

// Run starts the asynchronous processing of the workqueue for delegates
// synchronization events that will proceed until a shutdown signal is received.
// Individual delegates are started as they are created and are shutdown
// upon the same shutdown signal.
func (b *backend) Run(stopCh <-chan struct{}) error {
	// spawn a goroutine that block on waiting for shutdown signal
	// and will shutdown delegates upon receiving one and then exit.
	go func() {
		// Defer panic handling
		defer utilruntime.HandleCrash()

		// Start controller
		if err := b.auditSinkControler.run(stopCh); err != nil {
			return
		}

		<-stopCh
		b.stopAllDelegates()
	}()
	return nil
}

// stopAllDelegates closes the stopChan for every delegate to enable
// goroutines to terminate gracefully. This is a helper method to propagate
// the primary stopChan to the current delegate map.
func (b *backend) stopAllDelegates() {
	b.delegateUpdateMutex.Lock()
	for _, d := range b.GetDelegates() {
		close(d.stopChan)
	}
}

// Shutdown calls the shutdown method on all delegates. The stopChan should
// be closed before this is called.
func (b *backend) Shutdown() {
	for _, d := range b.GetDelegates() {
		d.Shutdown()
	}
}

// GetDelegates retrieves current delegates in a safe manner
func (b *backend) GetDelegates() syncedDelegates {
	return b.delegates.Load().(syncedDelegates)
}

// copyDelegates returns a copied delegate map
func (b *backend) copyDelegates() syncedDelegates {
	c := make(syncedDelegates)
	for u, s := range b.GetDelegates() {
		c[u] = s
	}
	return c
}

// setDelegates sets the current delegates in a safe manner
func (b *backend) setDelegates(delegates syncedDelegates) {
	b.delegates.Store(delegates)
}

// unregisterAndStopDelegate removes the delegate for a sink
// with this uid from the delegates list and invokes
// non-blocking graceful shutdown on it
func (b *backend) unregisterAndStopDelegate(uid types.UID) {
	b.delegateUpdateMutex.Lock()
	defer b.delegateUpdateMutex.Unlock()
	delegates := b.copyDelegates()
	if d, ok := delegates[uid]; ok {
		delete(delegates, uid)
		b.setDelegates(delegates)
		go d.gracefulShutdown()
		klog.V(2).Infof("Removed audit sink: %s", d.configuration.Name)
		klog.V(2).Infof("Current audit sinks: %v", delegates.Names())
	}
}

// registerAndRunNewDelegate creates a new delegate for a sink
// and updates the registry with it
func (b *backend) registerAndRunNewDelegate(sink *auditregv1alpha1.AuditSink) error {
	d, err := b.createAndStartDelegate(sink)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to create sink %s: %v", sink.Name, err))
		b.recorder.Event(sink, corev1.EventTypeWarning, "CreateFailed", fmt.Sprintf("Could not create audit sink %s: %v", sink.Name, err))
		return err
	}
	b.delegateUpdateMutex.Lock()
	defer b.delegateUpdateMutex.Unlock()
	delegates := b.copyDelegates()
	delegates[sink.UID] = d
	b.setDelegates(delegates)
	klog.V(2).Infof("Added audit sink: %s", sink.Name)
	klog.V(2).Infof("Current audit sinks: %v", delegates.Names())
	return nil
}

// createAndStartDelegate will build a delegate from an audit sink configuration and run it synchronously
func (b *backend) createAndStartDelegate(sink *auditregv1alpha1.AuditSink) (*delegate, error) {
	auditClasses, err := b.config.AuditClassInformer.Lister().List(labels.Everything())
	if err != nil {
		return nil, err
	}
	f := factory{
		config:               b.config,
		webhookClientManager: b.webhookClientManager,
		sink:                 sink,
		auditClasses:         auditClasses,
	}
	delegate, err := f.BuildDelegate()
	if err != nil {
		return nil, err
	}
	err = delegate.Run(delegate.stopChan)
	if err != nil {
		return nil, err
	}
	return delegate, nil
}

// String returns a string representation of the backend
func (b *backend) String() string {
	var delegateStrings []string
	for _, delegate := range b.GetDelegates() {
		delegateStrings = append(delegateStrings, fmt.Sprintf("%s", delegate))
	}
	return fmt.Sprintf("%s[%s]", PluginName, strings.Join(delegateStrings, ","))
}

type runner interface {
	run(stopCh <-chan struct{}) error
}

type auditSinkReconciler interface {
	reconcile(key uidKey) error
}

type defaultAuditSinkControl struct {
	delegateManager    delegateManager
	auditSinks         auditlisters.AuditSinkLister
	auditSinksSynced   cache.InformerSynced
	auditClasses       auditlisters.AuditClassLister
	auditClassesSynced cache.InformerSynced
	workqueue          workqueue.RateLimitingInterface
	workersCount       int
	// exposed as struct property only for test injections
	reconciler auditSinkReconciler
}

type uidKey struct {
	uid  types.UID
	name string
}

func sinkToKey(sink *auditregv1alpha1.AuditSink) (uidKey, error) {
	key, err := cache.MetaNamespaceKeyFunc(sink)
	return uidKey{
		uid:  sink.GetUID(),
		name: key,
	}, err
}

// newAuditSinkControler constructs a new controller instance to handle AuditSink and AuditClass resource events
func newAuditSinkControler(auditSinkInformer auditinformer.AuditSinkInformer,
	auditClassInformer auditinformer.AuditClassInformer,
	workersCount int,
	delegateManager delegateManager) runner {
	workqueue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AuditSinks")

	d := &defaultAuditSinkControl{
		delegateManager:    delegateManager,
		auditSinks:         auditSinkInformer.Lister(),
		auditSinksSynced:   auditSinkInformer.Informer().HasSynced,
		auditClasses:       auditClassInformer.Lister(),
		auditClassesSynced: auditClassInformer.Informer().HasSynced,
		workqueue:          workqueue,
		workersCount:       workersCount,
	}

	d.reconciler = d

	auditSinkInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    d.enqueueSink,
		UpdateFunc: d.updateSink,
		DeleteFunc: d.deleteSink,
	})

	auditClassInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    d.onAuditClassEvent,
		UpdateFunc: d.onAuditClassEventUpdate,
		DeleteFunc: d.onAuditClassEvent,
	})

	return d
}

func (d *defaultAuditSinkControl) run(stopCh <-chan struct{}) error {
	// Defer workqueue shutdown
	defer d.workqueue.ShutDown()
	// Defer panic handling
	defer utilruntime.HandleCrash()
	// Waiting for caches to sync for dynamic audit plugin
	if !cache.WaitForCacheSync(stopCh, d.auditSinksSynced, d.auditClassesSynced) {
		err := fmt.Errorf("unable to sync caches for dynamic audit plugin")
		utilruntime.HandleError(err)
		return err
	}

	//Starting workers
	for i := 0; i < d.workersCount; i++ {
		go wait.Until(d.runWorker, time.Second, stopCh)
	}

	<-stopCh

	return nil
}

// enqueueSink is called by the shared informer when an AuditSink resource is added or updated,
// or when a referenced AuditClass event occurs
func (d *defaultAuditSinkControl) enqueueSink(newObj interface{}) {
	sink, ok := newObj.(*auditregv1alpha1.AuditSink)
	if !ok {
		return
	}
	key, err := sinkToKey(sink)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get cache key for AuditSink object: %v", err))
		return
	}
	d.workqueue.Add(key)
}

// updateSink is called by the shared informer when an AuditSink is updated
func (d *defaultAuditSinkControl) updateSink(oldObj, newObj interface{}) {
	var (
		oldSink = oldObj.(*auditregv1alpha1.AuditSink)
		newSink = newObj.(*auditregv1alpha1.AuditSink)
	)
	// We want to make sure delegates are stopped ASAP. Finalizer changes are an update event and
	// resource might be in delete state already here, before watcher has triggered delete event.
	// Hence the check for `DeletionTimestamp` in this update handler.
	if newSink.DeletionTimestamp != nil ||
		!apiequality.Semantic.DeepEqual(oldSink.Spec, newSink.Spec) {
		d.enqueueSink(newSink)
		return
	}
}

// deleteSink is called by the shared informer when an AuditSink is deleted
func (d *defaultAuditSinkControl) deleteSink(obj interface{}) {
	sink, ok := obj.(*auditregv1alpha1.AuditSink)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type %#v", obj))
			return
		}
		if sink, ok = tombstone.Obj.(*auditregv1alpha1.AuditSink); !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type %#v", tombstone.Obj))
			return
		}
	}

	key, err := sinkToKey(sink)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get cache key for AuditSink object: %v", err))
		return
	}

	d.workqueue.Add(key)
}

// onAuditClassEvent is triggered by informer upon AuditClass
// lifecycle events to find out affected sinks and have them enqued
// for reconciliation
func (d *defaultAuditSinkControl) onAuditClassEvent(obj interface{}) {
	var (
		class *auditregv1alpha1.AuditClass
		ok    bool
		sinks []*auditregv1alpha1.AuditSink
		err   error
	)
	if class, ok = obj.(*auditregv1alpha1.AuditClass); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type %#v", obj))
			return
		}
		if class, ok = tombstone.Obj.(*auditregv1alpha1.AuditClass); !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type %#v", tombstone.Obj))
			return
		}
	}
	// list the latest info from API server for the AuditSink resources
	if sinks, err = d.auditSinks.List(labels.Everything()); err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to list AuditSink resources: %v", err))
	}
	// Search for references to this class in the policy rules of the sinks
	// and enque the sinks with positive hits for reconciliation
	for _, sink := range sinks {
		for _, rule := range sink.Spec.Policy.Rules {
			if rule.WithAuditClass == class.GetName() {
				d.enqueueSink(sink)
				break
			}
		}
	}
}

func (d *defaultAuditSinkControl) onAuditClassEventUpdate(oldObj, newObj interface{}) {
	var (
		oldClass = oldObj.(*auditregv1alpha1.AuditClass)
		newClass = newObj.(*auditregv1alpha1.AuditClass)
	)
	if newClass.DeletionTimestamp != nil ||
		!apiequality.Semantic.DeepEqual(oldClass.Spec, newClass.Spec) {
		d.onAuditClassEvent(newClass)
	}
}

// reconcile synchronizes the desired delegates state with the actual
func (d *defaultAuditSinkControl) reconcile(key uidKey) error {

	// Get the latest info from API server for the AuditSink resource with this name
	sink, err := d.auditSinks.Get(key.name)
	switch {
	case errors.IsNotFound(err):
		{
			// auditsink absence in store means watcher caught the deletion.
			// Shutdown and clear from registry
			d.delegateManager.unregisterAndStopDelegate(key.uid)
			return nil
		}
	case err != nil:
		utilruntime.HandleError(fmt.Errorf("Unable to retrieve auditsink %v from store: %v", key.name, err))
	default:
		{
			// if sink has been recreated too fast we may
			// end up with key for a stale object
			if sink.GetUID() != key.uid {
				d.delegateManager.unregisterAndStopDelegate(key.uid)
			}
			if sink.DeletionTimestamp != nil {
				// auditsink is being deleted. shutdown and clear the delegate from registry
				d.delegateManager.unregisterAndStopDelegate(sink.UID)
				return nil
			}
			// Create and start a new delegate for this sink UID
			err = d.delegateManager.registerAndRunNewDelegate(sink)
		}
	}

	return err
}

func (d *defaultAuditSinkControl) runWorker() {
	for d.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the `reconcile` function.
// Errors returned by the reconcile function will cause the key used for the
// faulty execution to be enqued for reconcilliation again (rate limited).
func (d *defaultAuditSinkControl) processNextWorkItem() bool {
	obj, shutdown := d.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer d.workqueue.Done(obj)
		var key uidKey
		var ok bool
		if key, ok = obj.(uidKey); !ok {
			d.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected type uidKey in workqueue but got %#v", obj))
			return nil
		}
		if err := d.reconciler.reconcile(key); err != nil {
			d.workqueue.AddRateLimited(key)
			return fmt.Errorf("failed to reconcile '%s': %s, requeuing", key.name, err.Error())
		}
		d.workqueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}
