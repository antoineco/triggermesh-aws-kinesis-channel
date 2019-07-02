/*
Copyright 2018 The Knative Authors

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

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/knative/eventing/pkg/apis/duck/v1alpha1"
	eventingduck "github.com/knative/eventing/pkg/apis/duck/v1alpha1"
	eventingv1alpha1 "github.com/knative/eventing/pkg/apis/eventing/v1alpha1"
	"github.com/knative/eventing/pkg/logging"
	"github.com/knative/eventing/pkg/provisioners"
	"github.com/triggermesh/aws-kinesis-provisioner/pkg/kinesisutil"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

const (
	// maxElements defines a maximum number of outstanding re-connect requests
	maxElements = 10
)

var (
	// retryInterval defines delay in seconds for the next attempt to reconnect to kinesis streaming server
	retryInterval = 1 * time.Second
)

// SubscriptionsSupervisor manages the state of Kinesis Streaming subscriptions
type SubscriptionsSupervisor struct {
	logger *zap.Logger

	receiver   *provisioners.MessageReceiver
	dispatcher *provisioners.MessageDispatcher

	subscriptionsMux sync.Mutex
	subscriptions    map[provisioners.ChannelReference]map[subscriptionReference]*kinesis.Consumer

	connect                chan struct{}
	accountAccessKeyID     string
	accountSecretAccessKey string
	region                 string
	streamName             string
	// kinesisConnMux is used to protect kinesisConn and kinesisConnInProgress during
	// the transition from not connected to connected states.
	kinesisConnMux        sync.Mutex
	kinesisConn           *kinesis.Kinesis
	kinesisConnInProgress bool

	hostToChannelMap atomic.Value
}

// NewDispatcher returns a new SubscriptionsSupervisor.
func NewDispatcher(accountAccessKeyID, accountSecretAccessKey, region, streamName string, logger *zap.Logger) (*SubscriptionsSupervisor, error) {
	d := &SubscriptionsSupervisor{
		logger:                 logger,
		dispatcher:             provisioners.NewMessageDispatcher(logger.Sugar()),
		connect:                make(chan struct{}, maxElements),
		accountAccessKeyID:     accountAccessKeyID,
		accountSecretAccessKey: accountSecretAccessKey,
		region:                 region,
		streamName:             streamName,
		subscriptions:          make(map[provisioners.ChannelReference]map[subscriptionReference]*kinesis.Consumer),
	}
	d.setHostToChannelMap(map[string]provisioners.ChannelReference{})
	receiver, err := provisioners.NewMessageReceiver(
		createReceiverFunction(d, logger.Sugar()),
		logger.Sugar(),
		provisioners.ResolveChannelFromHostHeader(provisioners.ResolveChannelFromHostFunc(d.getChannelReferenceFromHost)))
	if err != nil {
		return nil, err
	}
	d.receiver = receiver
	return d, nil
}

func (s *SubscriptionsSupervisor) signalReconnect() {
	select {
	case s.connect <- struct{}{}:
		// Sent.
	default:
		// The Channel is already full, so a reconnection attempt will occur.
	}
}

func createReceiverFunction(s *SubscriptionsSupervisor, logger *zap.SugaredLogger) func(provisioners.ChannelReference, *provisioners.Message) error {
	return func(channel provisioners.ChannelReference, m *provisioners.Message) error {
		logger.Infof("Received message from %q channel", channel.String())
		//publish to kinesis
		ch := getSubject(channel)
		message, err := json.Marshal(m)
		if err != nil {
			logger.Errorf("Error during marshaling of the message: %v", err)
			return err
		}
		s.kinesisConnMux.Lock()
		currentkinesisConn := s.kinesisConn
		s.kinesisConnMux.Unlock()
		if currentkinesisConn == nil {
			return fmt.Errorf("No Connection to kinesis")
		}
		if err := kinesisutil.Publish(currentkinesisConn, &ch, &s.streamName, message, logger); err != nil {
			logger.Errorf("Error during publish: %v", err)
			return err
		}
		logger.Infof("Published [%s] : '%s'", channel.String(), m.Headers)
		return nil
	}
}

func (s *SubscriptionsSupervisor) Start(stopCh <-chan struct{}) error {
	// Starting Connect to establish connection with Kinesis
	go s.Connect(stopCh)
	// Trigger Connect to establish connection with Kinesis
	s.signalReconnect()
	return s.receiver.Start(stopCh)
}

func (s *SubscriptionsSupervisor) connectWithRetry(stopCh <-chan struct{}) {
	// re-attempting evey 1 second until the connection is established.
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		kConn, err := kinesisutil.Connect(s.accountAccessKeyID, s.accountSecretAccessKey, s.region, s.streamName, s.logger.Sugar())
		if err == nil {
			// Locking here in order to reduce time in locked state.
			s.kinesisConnMux.Lock()
			s.kinesisConn = kConn
			s.kinesisConnInProgress = false
			s.kinesisConnMux.Unlock()
			return
		}
		s.logger.Sugar().Errorf("Connect() failed with error: %+v, retrying in %s", err, retryInterval.String())
		select {
		case <-ticker.C:
			continue
		case <-stopCh:
			return
		}
	}
}

// Connect is called for initial connection as well as after every disconnect
func (s *SubscriptionsSupervisor) Connect(stopCh <-chan struct{}) {
	for {
		select {
		case <-s.connect:
			s.kinesisConnMux.Lock()
			currentConnProgress := s.kinesisConnInProgress
			s.kinesisConnMux.Unlock()
			if !currentConnProgress {
				// Case for lost connectivity, setting InProgress to true to prevent recursion
				s.kinesisConnMux.Lock()
				s.kinesisConnInProgress = true
				s.kinesisConnMux.Unlock()
				go s.connectWithRetry(stopCh)
			}
		case <-stopCh:
			return
		}
	}
}

// UpdateSubscriptions creates/deletes the kinesis subscriptions based on channel.Spec.Subscribable.Subscribers
// Return type:map[eventingduck.SubscriberSpec]error --> Returns a map of subscriberSpec that failed with the value=error encountered.
// Ignore the value in case error != nil
func (s *SubscriptionsSupervisor) UpdateSubscriptions(channel *eventingv1alpha1.Channel, isFinalizer bool) (map[eventingduck.SubscriberSpec]error, error) {
	s.subscriptionsMux.Lock()
	defer s.subscriptionsMux.Unlock()

	failedToSubscribe := make(map[eventingduck.SubscriberSpec]error)
	cRef := provisioners.ChannelReference{Namespace: channel.Namespace, Name: channel.Name}
	if channel.Spec.Subscribable == nil || isFinalizer {
		s.logger.Sugar().Infof("Empty subscriptions for channel Ref: %v; unsubscribe all active subscriptions, if any", cRef)
		chMap, ok := s.subscriptions[cRef]
		if !ok {
			// nothing to do
			s.logger.Sugar().Infof("No channel Ref %v found in subscriptions map", cRef)
			return failedToSubscribe, nil
		}
		for sub := range chMap {
			s.unsubscribe(cRef, sub)
		}
		delete(s.subscriptions, cRef)
		return failedToSubscribe, nil
	}

	subscriptions := channel.Spec.Subscribable.Subscribers
	activeSubs := make(map[subscriptionReference]bool) // it's logically a set

	chMap, ok := s.subscriptions[cRef]
	if !ok {
		chMap = make(map[subscriptionReference]*kinesis.Consumer)
		s.subscriptions[cRef] = chMap
	}
	var errStrings []string
	for _, sub := range subscriptions {
		// check if the subscription already exist and do nothing in this case
		subRef := newSubscriptionReference(sub)
		if _, ok := chMap[subRef]; ok {
			activeSubs[subRef] = true
			s.logger.Sugar().Infof("Subscription: %v already active for channel: %v", sub, cRef)
			continue
		}
		// subscribe and update failedSubscription if subscribe fails
		kinesisSub, err := s.subscribe(cRef, subRef)
		if err != nil {
			errStrings = append(errStrings, err.Error())
			s.logger.Sugar().Errorf("failed to subscribe (subscription:%q) to channel: %v. Error:%s", sub, cRef, err.Error())
			failedToSubscribe[sub] = err
			continue
		}
		chMap[subRef] = kinesisSub
		activeSubs[subRef] = true
	}
	// Unsubscribe for deleted subscriptions
	for sub := range chMap {
		if ok := activeSubs[sub]; !ok {
			s.unsubscribe(cRef, sub)
		}
	}
	// delete the channel from s.subscriptions if chMap is empty
	if len(s.subscriptions[cRef]) == 0 {
		delete(s.subscriptions, cRef)
	}
	return failedToSubscribe, nil
}

func toSubscriberStatus(subSpec *v1alpha1.SubscriberSpec, condition corev1.ConditionStatus, msg string) *v1alpha1.SubscriberStatus {
	if subSpec == nil {
		return nil
	}
	return &v1alpha1.SubscriberStatus{
		UID:                subSpec.UID,
		ObservedGeneration: subSpec.Generation,
		Message:            msg,
		Ready:              condition,
	}
}

func (s *SubscriptionsSupervisor) subscribe(channel provisioners.ChannelReference, subscription subscriptionReference) (*kinesis.Consumer, error) {
	s.logger.Info("Subscribe to channel:", zap.Any("channel", channel), zap.Any("subscription", subscription))

	// mcb := func(msg *kinesis.Record) {
	// 	message := provisioners.Message{}
	// 	if err := json.Unmarshal(msg.Data, &message); err != nil {
	// 		s.logger.Error("Failed to unmarshal message: ", zap.Error(err))
	// 		return
	// 	}
	// 	s.logger.Sugar().Infof("kinesis message received from shard: %v; sequence: %v; timestamp: %v, encryption: '%s'", msg.PartitionKey, msg.SequenceNumber, msg.ApproximateArrivalTimestamp, msg.EncryptionType)
	// 	if err := s.dispatcher.DispatchMessage(&message, subscription.SubscriberURI, subscription.ReplyURI, provisioners.DispatchDefaults{Namespace: channel.Namespace}); err != nil {
	// 		s.logger.Error("Failed to dispatch message: ", zap.Error(err))
	// 		return
	// 	}
	// }
	// // subscribe to a kinesis subject
	// ch := getSubject(channel)
	// sub := subscription.String()
	// s.kinesisConnMux.Lock()
	// currentkinesisConn := s.kinesisConn
	// s.kinesisConnMux.Unlock()
	// if currentkinesisConn == nil {
	// 	return nil, fmt.Errorf("No Connection to kinesis")
	// }
	// need to create kinesis subscription
	return nil, nil
}

// should be called only while holding subscriptionsMux
func (s *SubscriptionsSupervisor) unsubscribe(channel provisioners.ChannelReference, subscription subscriptionReference) error {
	s.logger.Info("Unsubscribe from channel:", zap.Any("channel", channel), zap.Any("subscription", subscription))

	// if stanSub, ok := s.subscriptions[channel][subscription]; ok {
	// 	// delete from kinesis
	// 	if err := (*stanSub).Unsubscribe(); err != nil {
	// 		s.logger.Error("Unsubscribing kinesis Streaming subscription failed: ", zap.Error(err))
	// 		return err
	// 	}
	// 	delete(s.subscriptions[channel], subscription)
	// }
	return nil
}

func getSubject(channel provisioners.ChannelReference) string {
	return channel.Name + "." + channel.Namespace
}

func (s *SubscriptionsSupervisor) getHostToChannelMap() map[string]provisioners.ChannelReference {
	return s.hostToChannelMap.Load().(map[string]provisioners.ChannelReference)
}

func (s *SubscriptionsSupervisor) setHostToChannelMap(hcMap map[string]provisioners.ChannelReference) {
	s.hostToChannelMap.Store(hcMap)
}

// UpdateHostToChannelMap will be called from the controller that watches kinesis channels.
// It will update internal hostToChannelMap which is used to resolve the hostHeader of the
// incoming request to the correct ChannelReference in the receiver function.
func (s *SubscriptionsSupervisor) UpdateHostToChannelMap(ctx context.Context, chanList []eventingv1alpha1.Channel) error {
	hostToChanMap, err := provisioners.NewHostNameToChannelRefMap(chanList)
	if err != nil {
		logging.FromContext(ctx).Info("UpdateHostToChannelMap: Error occurred when creating the new hostToChannel map.", zap.Error(err))
		return err
	}
	s.setHostToChannelMap(hostToChanMap)
	logging.FromContext(ctx).Info("hostToChannelMap updated successfully.")
	return nil
}

func (s *SubscriptionsSupervisor) getChannelReferenceFromHost(host string) (provisioners.ChannelReference, error) {
	chMap := s.getHostToChannelMap()
	cr, ok := chMap[host]
	if !ok {
		return cr, fmt.Errorf("Invalid HostName:%q. HostName not found in any of the watched kinesis channels", host)
	}
	return cr, nil
}
