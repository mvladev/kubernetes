/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless assertd by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dynamic

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	auditregv1alpha1 "k8s.io/api/auditregistration/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/apiserver/pkg/audit"
	webhook "k8s.io/apiserver/pkg/util/webhook"
	informers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
)

var (
	alwaysReady        = func() bool { return true }
	noResyncPeriodFunc = func() time.Duration { return 0 }
	webhookURL         = "https://test.namespace.svc.cluster.local"
	webhookURL1        = "https://test1.namespace.svc.cluster.local"
)

func TestDynamicDelegates(t *testing.T) {
	eventList1 := &atomic.Value{}
	eventList1.Store(auditinternal.EventList{})
	eventList2 := &atomic.Value{}
	eventList2.Store(auditinternal.EventList{})

	// start test servers
	server1 := httptest.NewServer(buildTestHandler(t, eventList1))
	defer server1.Close()
	server2 := httptest.NewServer(buildTestHandler(t, eventList2))
	defer server2.Close()

	testPolicy := auditregv1alpha1.Policy{
		Level: auditregv1alpha1.LevelMetadata,
		Stages: []auditregv1alpha1.Stage{
			auditregv1alpha1.StageResponseStarted,
		},
	}
	testEvent := auditinternal.Event{
		Level:      auditinternal.LevelMetadata,
		Stage:      auditinternal.StageResponseStarted,
		Verb:       "get",
		RequestURI: "/test/path",
	}
	testConfig1 := &auditregv1alpha1.AuditSink{
		TypeMeta: metav1.TypeMeta{APIVersion: auditregv1alpha1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test1",
			UID:  types.UID("test1"),
		},
		Spec: auditregv1alpha1.AuditSinkSpec{
			Policy: testPolicy,
			Webhook: auditregv1alpha1.Webhook{
				ClientConfig: auditregv1alpha1.WebhookClientConfig{
					URL: &server1.URL,
				},
			},
		},
	}
	testConfig2 := &auditregv1alpha1.AuditSink{
		TypeMeta: metav1.TypeMeta{APIVersion: auditregv1alpha1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test2",
			UID:  types.UID("test2"),
		},
		Spec: auditregv1alpha1.AuditSinkSpec{
			Policy: testPolicy,
			Webhook: auditregv1alpha1.Webhook{
				ClientConfig: auditregv1alpha1.WebhookClientConfig{
					URL: &server2.URL,
				},
			},
		},
	}

	badURL := "http://badtest"
	badConfig := &auditregv1alpha1.AuditSink{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad",
			UID:  types.UID("bad"),
		},
		Spec: auditregv1alpha1.AuditSinkSpec{
			Policy: testPolicy,
			Webhook: auditregv1alpha1.Webhook{
				ClientConfig: auditregv1alpha1.WebhookClientConfig{
					URL: &badURL,
				},
			},
		},
	}

	t.Run("find none", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(stopCh)
		defer close(syncCalls)

		assert.Len(t, b.GetDelegates(), 0)
	})

	t.Run("find one", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(stopCh)
		defer close(syncCalls)

		config.AuditSinkInformer.Informer().GetStore().Add(testConfig1)
		controller := b.auditSinkControler.(*defaultAuditSinkControl)
		controller.enqueueSink(testConfig1)
		err := waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		delegates := b.GetDelegates()
		assert.Len(t, delegates, 1)
		assert.Contains(t, delegates, types.UID("test1"))
		assert.Equal(t, testConfig1, delegates["test1"].configuration)

		// send event and check that it arrives
		b.ProcessEvents(&testEvent)
		err = checkForEvent(eventList1, testEvent)
		assert.NoError(t, err, "unable to find events sent to sink")
	})

	// test that a bad webhook configuration can be recovered from
	t.Run("can delete bad config with events sent", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(stopCh)
		defer close(syncCalls)

		config.AuditSinkInformer.Informer().GetStore().Add(testConfig1)
		controller := b.auditSinkControler.(*defaultAuditSinkControl)
		controller.enqueueSink(testConfig1)
		err := waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		config.AuditSinkInformer.Informer().GetStore().Add(badConfig)
		controller.enqueueSink(badConfig)
		err = waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		delegates := b.GetDelegates()
		assert.Len(t, delegates, 2)
		assert.Contains(t, delegates, types.UID("bad"))
		assert.Equal(t, badConfig, delegates["bad"].configuration)

		// send events to the buffer
		b.ProcessEvents(&testEvent, &testEvent)

		// event is in the buffer see if the sink can be deleted
		// this will hang and fail if not handled properly
		config.AuditSinkInformer.Informer().GetStore().Delete(badConfig)
		b.auditSinkControler.(*defaultAuditSinkControl).deleteSink(badConfig)

		err = waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		delegates = b.GetDelegates()
		assert.Len(t, delegates, 1)
		assert.Contains(t, delegates, types.UID("test1"))
	})

	t.Run("find two", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(stopCh)
		defer close(syncCalls)
		eventList1.Store(auditinternal.EventList{})
		config.AuditSinkInformer.Informer().GetStore().Add(testConfig1)
		controller := b.auditSinkControler.(*defaultAuditSinkControl)
		controller.enqueueSink(testConfig1)
		err := waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		config.AuditSinkInformer.Informer().GetStore().Add(testConfig2)
		controller.enqueueSink(testConfig2)
		err = waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		delegates := b.GetDelegates()
		assert.Len(t, delegates, 2)
		assert.Contains(t, delegates, types.UID("test1"))
		assert.Contains(t, delegates, types.UID("test2"))
		assert.Equal(t, testConfig1, delegates["test1"].configuration)
		assert.Equal(t, testConfig2, delegates["test2"].configuration)

		// send event to both delegates and check that it arrives in both places
		b.ProcessEvents(&testEvent)
		err = checkForEvent(eventList1, testEvent)
		assert.NoError(t, err, "unable to find events sent to sink 1")
		err = checkForEvent(eventList2, testEvent)
		assert.NoError(t, err, "unable to find events sent to sink 2")
	})

	t.Run("delete one", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(stopCh)
		defer close(syncCalls)
		eventList2.Store(auditinternal.EventList{})
		config.AuditSinkInformer.Informer().GetStore().Add(testConfig1)
		controller := b.auditSinkControler.(*defaultAuditSinkControl)
		controller.enqueueSink(testConfig1)
		err := waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		config.AuditSinkInformer.Informer().GetStore().Add(testConfig2)
		controller.enqueueSink(testConfig2)
		err = waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		config.AuditSinkInformer.Informer().GetStore().Delete(testConfig1)
		controller.deleteSink(testConfig1)
		err = waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		delegates := b.GetDelegates()
		assert.Len(t, delegates, 1)
		assert.Contains(t, delegates, types.UID("test2"))
		assert.Equal(t, testConfig2, delegates["test2"].configuration)

		// send event and check that it arrives to remaining sink
		b.ProcessEvents(&testEvent)
		err = checkForEvent(eventList2, testEvent)
		assert.NoError(t, err, "unable to find events sent to sink")
	})

	t.Run("update one", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(stopCh)
		defer close(syncCalls)
		eventList2.Store(auditinternal.EventList{})
		config.AuditSinkInformer.Informer().GetStore().Add(testConfig1)
		controller := b.auditSinkControler.(*defaultAuditSinkControl)
		controller.enqueueSink(testConfig1)
		err := waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		oldConfig := *testConfig1
		// change back the shared testcofnig1 modification after test
		defer func() {
			testConfig1.Spec.Webhook.ClientConfig.URL = &server1.URL
		}()
		// modify testcofnig1 to redirect to server2 URL
		testConfig1.Spec.Webhook.ClientConfig.URL = &server2.URL
		config.AuditSinkInformer.Informer().GetStore().Update(testConfig1)
		controller.updateSink(&oldConfig, testConfig1)
		err = waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		delegates := b.GetDelegates()
		assert.Len(t, delegates, 1)
		assert.Contains(t, delegates, types.UID("test1"))
		assert.Equal(t, testConfig1, delegates["test1"].configuration)

		// send event and check that it arrives to updated sink
		b.ProcessEvents(&testEvent)
		err = checkForEvent(eventList2, testEvent)
		assert.NoError(t, err, "unable to find events sent to sink")
	})

	t.Run("update meta only", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(stopCh)
		defer close(syncCalls)
		eventList1.Store(auditinternal.EventList{})
		config.AuditSinkInformer.Informer().GetStore().Add(testConfig1)
		controller := b.auditSinkControler.(*defaultAuditSinkControl)
		controller.enqueueSink(testConfig1)
		err := waitFor(syncCalls, 2*time.Second)
		assert.NoError(t, err)

		oldConfig := *testConfig1
		testConfig1.Labels = map[string]string{"my": "label"}
		controller.updateSink(&oldConfig, testConfig1)
		err = waitFor(syncCalls, 2*time.Second)
		// exect never to be called on syncCalls
		assert.EqualError(t, err, wait.ErrWaitTimeout.Error())

		delegates := b.GetDelegates()
		assert.Len(t, delegates, 1)
		// send event and check that it arrives to same sink
		b.ProcessEvents(&testEvent)
		err = checkForEvent(eventList1, testEvent)
		assert.NoError(t, err, "unable to find events sent to sink")
	})

	t.Run("shutdown", func(t *testing.T) {
		b, config := newTestBackend()
		config.BufferedConfig.MaxBatchSize = 1
		stopCh, syncCalls := run(t, b)
		defer close(syncCalls)
		// if the stop signal is not propagated correctly the buffers will not
		// close down gracefully, and the shutdown method will hang causing
		// the test will timeout.
		timeoutChan := make(chan struct{})
		successChan := make(chan struct{})
		go func() {
			time.Sleep(1 * time.Second)
			timeoutChan <- struct{}{}
		}()
		go func() {
			close(stopCh)
			b.Shutdown()
			successChan <- struct{}{}
		}()
		for {
			select {
			case <-timeoutChan:
				t.Error("shutdown timed out")
				return
			case <-successChan:
				return
			}
		}
	})
}

// checkForEvent will poll to check for an audit event in an atomic event list
func checkForEvent(a *atomic.Value, evSent auditinternal.Event) error {
	return wait.Poll(100*time.Millisecond, 1*time.Second, func() (bool, error) {
		el := a.Load().(auditinternal.EventList)
		if len(el.Items) != 1 {
			return false, nil
		}
		evFound := el.Items[0]
		eq := reflect.DeepEqual(evSent, evFound)
		if !eq {
			return false, fmt.Errorf("event mismatch -- sent: %+v found: %+v", evSent, evFound)
		}
		return true, nil
	})
}

// buildTestHandler returns a handler that will update the atomic value passed in
// with the event list it receives
func buildTestHandler(t *testing.T, a *atomic.Value) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("could not read request body: %v", err)
		}
		el := auditinternal.EventList{}
		decoder := audit.Codecs.UniversalDecoder(auditv1.SchemeGroupVersion)
		if err := runtime.DecodeInto(decoder, body, &el); err != nil {
			t.Fatalf("failed decoding buf: %b, apiVersion: %s", body, auditv1.SchemeGroupVersion)
		}
		defer r.Body.Close()
		a.Store(el)
		w.WriteHeader(200)
	})
}

// defaultTestConfig returns a Config object suitable for testing along with its
// associated stopChan
func defaultTestConfig() *Config {
	authWrapper := webhook.AuthenticationInfoResolverWrapper(
		func(a webhook.AuthenticationInfoResolver) webhook.AuthenticationInfoResolver { return a },
	)
	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, noResyncPeriodFunc())

	eventSink := &v1core.EventSinkImpl{Interface: client.CoreV1().Events("")}

	return &Config{
		AuditSinkInformer:  informerFactory.Auditregistration().V1alpha1().AuditSinks(),
		AuditClassInformer: informerFactory.Auditregistration().V1alpha1().AuditClasses(),
		EventConfig:        EventConfig{Sink: eventSink},
		BufferedConfig:     NewDefaultWebhookBatchConfig(),
		WebhookConfig: WebhookConfig{
			AuthInfoResolverWrapper: authWrapper,
			ServiceResolver:         webhook.NewDefaultServiceResolver(),
		},
		WorkersCount: 10,
	}
}

func newTestBackend() (*backend, *Config) {
	var (
		b          *backend
		controller *defaultAuditSinkControl
		ok         bool
	)

	config := defaultTestConfig()
	dynamicBackend, _ := NewBackend(config)

	if b, ok = dynamicBackend.(*backend); ok {
		b.recorder = record.NewFakeRecorder(100)
		if controller, ok = b.auditSinkControler.(*defaultAuditSinkControl); ok {
			controller.auditSinksSynced = alwaysReady
			controller.auditClassesSynced = alwaysReady
		}
		return b, config
	}

	return nil, nil
}

var testPolicy = auditregv1alpha1.Policy{
	Level: auditregv1alpha1.LevelMetadata,
	Stages: []auditregv1alpha1.Stage{
		auditregv1alpha1.StageResponseStarted,
	},
}

func newAuditSink(name string, webhookurl *string, policy auditregv1alpha1.Policy) *auditregv1alpha1.AuditSink {
	sink := &auditregv1alpha1.AuditSink{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  uuid.NewUUID(),
		},
		Spec: auditregv1alpha1.AuditSinkSpec{
			Policy: policy,
			Webhook: auditregv1alpha1.Webhook{
				ClientConfig: auditregv1alpha1.WebhookClientConfig{
					URL: webhookurl,
				},
			},
		},
	}
	return sink
}

func newAuditClass(name string) *auditregv1alpha1.AuditClass {
	class := &auditregv1alpha1.AuditClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  uuid.NewUUID(),
		},
		Spec: auditregv1alpha1.AuditClassSpec{
			RequestSelectors: []auditregv1alpha1.RequestSelector{
				{
					Users:           []string{"system:kube-proxy"},
					UserGroups:      []string{"system:authenticated"},
					Verbs:           []string{"watch"},
					Namespaces:      []string{"test"},
					NonResourceURLs: []string{"/status"},
					Resources: []auditregv1alpha1.GroupResources{
						{
							Group:       "",
							Resources:   []string{"configmaps", "secrets"},
							ObjectNames: []string{"controller-leader"},
						},
					},
				},
			},
		},
	}
	return class
}

type wrapper struct {
	t         *testing.T
	original  *defaultAuditSinkControl
	syncCalls chan struct{}
}

func (w *wrapper) reconcile(key uidKey) error {
	err := w.original.reconcile(key)
	if err != nil {
		w.t.Logf("%v", err)
	}
	w.syncCalls <- struct{}{}
	return err
}

func run(t *testing.T, b *backend) (chan struct{}, chan struct{}) {
	syncCalls := make(chan struct{})
	w := &wrapper{
		t:         t,
		original:  b.auditSinkControler.(*defaultAuditSinkControl),
		syncCalls: syncCalls,
	}

	b.auditSinkControler.(*defaultAuditSinkControl).reconciler = w

	stopCh := make(chan struct{})
	go b.Run(stopCh)

	return stopCh, syncCalls
}

func TestAuditSinkAddControl(t *testing.T) {

	b, config := newTestBackend()
	sinkStore := config.AuditSinkInformer.Informer().GetStore()

	stopCh, syncCalls := run(t, b)
	defer close(stopCh)
	defer close(syncCalls)

	auditSink := newAuditSink("test", &webhookURL, testPolicy)
	sinkStore.Add(auditSink)

	controller := b.auditSinkControler.(*defaultAuditSinkControl)
	controller.enqueueSink(auditSink)

	err := waitFor(syncCalls, 2*time.Second)
	assert.NoError(t, err)
	assert.NotNil(t, b.GetDelegates()[auditSink.GetUID()])
	assert.Equal(t, auditSink, b.GetDelegates()[auditSink.GetUID()].configuration)
	assert.True(t, len(b.GetDelegates()) == 1)
}

func TestAuditSinkUpdateControl(t *testing.T) {

	b, config := newTestBackend()
	sinkStore := config.AuditSinkInformer.Informer().GetStore()

	stopCh, syncCalls := run(t, b)
	defer close(stopCh)
	defer close(syncCalls)

	auditSink := newAuditSink("test", &webhookURL, testPolicy)
	sinkStore.Add(auditSink)
	auditSinkUpdate := auditSink.DeepCopy()
	auditSinkUpdate.Spec.Webhook.ClientConfig.URL = &webhookURL1
	sinkStore.Update(auditSinkUpdate)

	controller := b.auditSinkControler.(*defaultAuditSinkControl)
	controller.updateSink(auditSink, auditSinkUpdate)

	err := waitFor(syncCalls, 2*time.Second)
	assert.NoError(t, err)
	assert.NotNil(t, b.GetDelegates()[auditSink.GetUID()])
	assert.Equal(t, auditSinkUpdate, b.GetDelegates()[auditSink.GetUID()].configuration)
	assert.True(t, len(b.GetDelegates()) == 1)
}

func TestAuditSinkDeleteControl(t *testing.T) {

	b, config := newTestBackend()
	sinkStore := config.AuditSinkInformer.Informer().GetStore()

	stopCh, syncCalls := run(t, b)
	defer close(stopCh)
	defer close(syncCalls)

	auditSink := newAuditSink("test", &webhookURL, testPolicy)
	now := metav1.Now()
	auditSink.DeletionTimestamp = &now
	sinkStore.Add(auditSink)

	controller := b.auditSinkControler.(*defaultAuditSinkControl)
	controller.deleteSink(auditSink)

	err := waitFor(syncCalls, 2*time.Second)
	assert.NoError(t, err)
	assert.True(t, len(b.GetDelegates()) == 0)
}

func TestAuditClassAddControl(t *testing.T) {

	b, config := newTestBackend()
	classStore := config.AuditClassInformer.Informer().GetStore()
	sinkStore := config.AuditSinkInformer.Informer().GetStore()

	stopCh, syncCalls := run(t, b)
	defer close(stopCh)
	defer close(syncCalls)

	auditClass := newAuditClass("testAuditClass")
	testPolicyWithClass := auditregv1alpha1.Policy{
		Level: auditregv1alpha1.LevelMetadata,
		Stages: []auditregv1alpha1.Stage{
			auditregv1alpha1.StageResponseStarted,
		},
	}
	testPolicyWithClass.Rules = []auditregv1alpha1.PolicyRule{
		{
			WithAuditClass: "testAuditClass",
			Level:          auditregv1alpha1.LevelRequest,
			Stages: []auditregv1alpha1.Stage{
				auditregv1alpha1.StageRequestReceived,
				auditregv1alpha1.StageResponseStarted,
			},
		},
	}
	auditSink := newAuditSink("test", &webhookURL, testPolicyWithClass)

	classStore.Add(auditClass)
	sinkStore.Add(auditSink)

	controller := b.auditSinkControler.(*defaultAuditSinkControl)
	controller.onAuditClassEvent(auditClass)

	err := waitFor(syncCalls, 2*time.Second)
	assert.NoError(t, err)
	//TODO : check if update invoked
	assert.True(t, len(b.GetDelegates()) == 1)
}

// blocks waiting until signal recevied on channel, channel closed
// or timeout
func waitFor(ch <-chan struct{}, timeout time.Duration) error {
	for {
		select {
		case _, open := <-ch:
			if !open {
				return wait.ErrWaitTimeout
			}
			return nil
		case <-time.After(timeout):
			{
				return wait.ErrWaitTimeout
			}
		}
	}
}

func TestNewBackend(t *testing.T) {
	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, noResyncPeriodFunc())
	auditSinkInformer := informerFactory.Auditregistration().V1alpha1().AuditSinks()
	auditClassInformer := informerFactory.Auditregistration().V1alpha1().AuditClasses()

	eventSink := &v1core.EventSinkImpl{Interface: client.CoreV1().Events("")}

	authWrapper := webhook.AuthenticationInfoResolverWrapper(
		func(a webhook.AuthenticationInfoResolver) webhook.AuthenticationInfoResolver { return a },
	)
	serviceResolver := webhook.NewDefaultServiceResolver()

	tcs := map[string]*Config{
		"should be ok with complete config": &Config{
			AuditSinkInformer:  auditSinkInformer,
			AuditClassInformer: auditClassInformer,
			EventConfig:        EventConfig{Sink: eventSink},
			BufferedConfig:     NewDefaultWebhookBatchConfig(),
			WebhookConfig: WebhookConfig{
				AuthInfoResolverWrapper: authWrapper,
				ServiceResolver:         serviceResolver,
			},
			WorkersCount: 10,
		},
		"should be ok without config properties with defaults": &Config{
			AuditSinkInformer:  auditSinkInformer,
			AuditClassInformer: auditClassInformer,
			EventConfig:        EventConfig{Sink: eventSink},
			WebhookConfig: WebhookConfig{
				AuthInfoResolverWrapper: authWrapper,
				ServiceResolver:         serviceResolver,
			},
			WorkersCount: 10,
		},
	}

	for desc, cfg := range tcs {
		t.Run(desc, func(t *testing.T) {
			//save initial config state before defaults apply
			initialCfg := *cfg
			b, err := NewBackend(cfg)
			assert.NoError(t, err)
			assert.NotNil(t, b)
			if _b, ok := b.(*backend); ok {
				// Assert default when no BufferedConfig provided
				if initialCfg.BufferedConfig == nil {
					assert.Equal(t, _b.config.BufferedConfig, NewDefaultWebhookBatchConfig())
				}
			}
		})
	}
}
