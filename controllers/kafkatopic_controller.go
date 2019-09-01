// Copyright © 2019 Banzai Cloud
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

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	v1alpha1 "github.com/banzaicloud/kafka-operator/api/v1alpha1"
	"github.com/banzaicloud/kafka-operator/pkg/k8sutil"
	"github.com/banzaicloud/kafka-operator/pkg/kafkautil"
	"github.com/banzaicloud/kafka-operator/pkg/util"
	logr "github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var topicFinalizer = "finalizer.kafkatopics.kafka.cbs-seattle.com"
var syncRoutines = make(map[string]struct{}, 0)

func SetupKafkaTopicWithManager(mgr ctrl.Manager) error {
	// Create a new controller
	r := &KafkaTopicReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("KafkaTopic"),
	}

	c, err := controller.New("kafkatopic", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource KafkaTopic
	err = c.Watch(&source.Kind{Type: &v1alpha1.KafkaTopic{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that KafkaTopicReconciler implements reconcile.Reconciler
var _ reconcile.Reconciler = &KafkaTopicReconciler{}

// KafkaTopicReconciler reconciles a KafkaTopic object
type KafkaTopicReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	Client client.Client
	Log    logr.Logger
}

// +kubebuilder:rbac:groups=banzaicloud.banzaicloud.io,resources=kafkatopics,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=banzaicloud.banzaicloud.io,resources=kafkatopics/status,verbs=get;update;patch

// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *KafkaTopicReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := r.Log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling KafkaTopic")
	var err error

	// Fetch the KafkaTopic instance
	instance := &v1alpha1.KafkaTopic{}
	if err = r.Client.Get(context.TODO(), request.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Get the referenced kafkacluster
	if instance.Spec.ClusterRef.Namespace == "" {
		instance.Spec.ClusterRef.Namespace = instance.Namespace
	}
	var cluster *v1alpha1.KafkaCluster
	if cluster, err = k8sutil.LookupKafkaCluster(r.Client, instance.Spec.ClusterRef); err != nil {
		reqLogger.Error(err, "Failed to lookup referenced cluster")
		return reconcile.Result{}, err
	}

	// Build a kafka config from the cluster spec
	kafkaConfig, err := kafkautil.ClusterConfig(r.Client, cluster)
	if err != nil {
		reqLogger.Error(err, "Failed to retrieve kafka config from cluster")
		return reconcile.Result{}, err
	}

	// Get a kafka connection
	reqLogger.Info("Retrieving kafka admin client")
	broker, err := kafkautil.New(kafkaConfig)
	if err != nil {
		reqLogger.Error(err, "Error connecting to kafka")
		return reconcile.Result{}, err
	}
	defer broker.Close()

	// Check if marked for deletion and run finalizers
	isMarkedForDeletion := instance.GetDeletionTimestamp() != nil
	if isMarkedForDeletion {
		return r.checkFinalizers(reqLogger, broker, instance)
	}

	// Check if the topic already exists
	existing, err := broker.GetTopic(instance.Spec.Name)
	if err != nil {
		return reconcile.Result{}, err
	}

	// we got a topic back
	if existing != nil {
		// check if requesting a partition decrease, we can't do this
		reqLogger.Info("Topic already exists, verifying configuration")
		if existing.NumPartitions > instance.Spec.Partitions {
			reqLogger.Info(fmt.Sprint("IGNORING: Spec is requesting partition decrease from", existing.NumPartitions, "to", instance.Spec.Partitions))
		} else {
			// Ensure partition count and topic configurations
			if changed, err := broker.EnsurePartitionCount(instance.Spec.Name, instance.Spec.Partitions); err != nil {
				return reconcile.Result{}, err
			} else if changed {
				reqLogger.Info("Increased partition count for topic")
			}
		}
		if err = broker.EnsureTopicConfig(instance.Spec.Name, util.MapStringStringPointer(instance.Spec.Config)); err != nil {
			return reconcile.Result{}, err
		}
		reqLogger.Info("Verified partitions and configuration for topic")

	} else {

		// Create the topic
		if err = broker.CreateTopic(&kafkautil.CreateTopicOptions{
			Name:              instance.Spec.Name,
			Partitions:        instance.Spec.Partitions,
			ReplicationFactor: int16(instance.Spec.ReplicationFactor),
			Config:            util.MapStringStringPointer(instance.Spec.Config),
		}); err != nil {
			reqLogger.Error(err, "Failed to create KafkaTopic")
			return reconcile.Result{}, err
		}

	}

	// ensure a finalizer for cleanup on deletion
	if !contains(instance.GetFinalizers(), topicFinalizer) {
		if err = r.addFinalizer(reqLogger, instance); err != nil {
			return reconcile.Result{}, err
		}
	}

	if _, ok := syncRoutines[instance.Name]; !ok {
		reqLogger.Info("Starting status sync routine for topic")
		syncRoutines[instance.Name] = struct{}{}
		go r.syncTopicStatus(kafkaConfig, instance)
	}

	reqLogger.Info("Ensured topic")

	return reconcile.Result{}, nil
}

func (r *KafkaTopicReconciler) syncTopicStatus(kafkaConfig *kafkautil.KafkaConfig, instance *v1alpha1.KafkaTopic) {
	syncLogger := r.Log.WithName(fmt.Sprintf("%s_sync", instance.Name))
	ticker := time.NewTicker(time.Duration(15) * time.Second)
	for _ = range ticker.C {
		syncLogger.Info("Syncing topic status")
		if cont := r.doTopicStatusSync(syncLogger, kafkaConfig, instance); !cont {
			if _, ok := syncRoutines[instance.Name]; ok {
				delete(syncRoutines, instance.Name)
			}
			return
		}
	}
}

func (r *KafkaTopicReconciler) doTopicStatusSync(syncLogger logr.Logger, kafkaConfig *kafkautil.KafkaConfig, instance *v1alpha1.KafkaTopic) bool {
	// grab a connection to kafka
	k, err := kafkautil.New(kafkaConfig)
	if err != nil {
		syncLogger.Error(err, "Failed to get a broker connection to update topic status")
		// let's still try again later, in case it was just a blib in availability
		return true
	}
	defer k.Close()

	// check if the topic still exists
	topic := &v1alpha1.KafkaTopic{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, topic); err != nil {
		syncLogger.Info("Topic has been deleted, stopping sync routine")
		// return false to stop calling goroutine
		return false
	}

	// get topic metadata
	meta, err := k.DescribeTopic(topic.Name)
	if err != nil {
		syncLogger.Error(err, "Failed to describe topic to update its status")
		return true
	}

	// iterate topic partitions and populate maps with their values
	leaders := make(map[string]string, 0)
	replicaCounts := make(map[string]string, 0)
	isr := make(map[string]string, 0)
	offlineReplicas := make(map[string]string, 0)

	for _, part := range meta.Partitions {
		ID := strconv.Itoa(int(part.ID))

		leaders[ID] = fmt.Sprintf("%s/%s", strconv.Itoa(int(part.Leader)), k.ResolveBrokerID(part.Leader))
		replicaCounts[ID] = strconv.Itoa(len(part.Replicas))

		if len(part.Isr) > 0 {
			isr[ID] = fmt.Sprintf("%+v", part.Isr)
		}

		if len(part.OfflineReplicas) > 0 {
			offlineReplicas[ID] = fmt.Sprintf("%+v", part.OfflineReplicas)
		}

	}
	// finally update status
	updated := topic.DeepCopy()
	updated.Status = v1alpha1.KafkaTopicStatus{
		PartitionCount:  int32(len(meta.Partitions)),
		Leaders:         leaders,
		ReplicaCounts:   replicaCounts,
		InSyncReplicas:  isr,
		OfflineReplicas: offlineReplicas,
	}
	if err = r.Client.Status().Update(context.TODO(), updated); err != nil {
		syncLogger.Error(err, "Failed to update KafkaTopic status")
	}
	return true

}

func (r *KafkaTopicReconciler) checkFinalizers(reqLogger logr.Logger, broker kafkautil.KafkaClient, topic *v1alpha1.KafkaTopic) (reconcile.Result, error) {
	reqLogger.Info("Kafka topic is marked for deletion")
	if contains(topic.GetFinalizers(), topicFinalizer) {
		if err := r.finalizeKafkaTopic(reqLogger, broker, topic); err != nil {
			reqLogger.Error(err, "Failed to finalize kafka topic")
			return reconcile.Result{}, err
		}
		topic.SetFinalizers(remove(topic.GetFinalizers(), topicFinalizer))
		if err := r.Client.Update(context.TODO(), topic); err != nil {
			reqLogger.Error(err, "Failed to update finalizers for kafka topic")
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (r *KafkaTopicReconciler) finalizeKafkaTopic(reqLogger logr.Logger, broker kafkautil.KafkaClient, topic *v1alpha1.KafkaTopic) error {
	exists, err := broker.GetTopic(topic.Spec.Name)
	if err != nil {
		return err
	}
	if exists != nil {
		if err = broker.DeleteTopic(topic.Spec.Name); err != nil {
			return err
		}
		reqLogger.Info("Deleted topic")
	}
	return nil
}

func (r *KafkaTopicReconciler) addFinalizer(reqLogger logr.Logger, topic *v1alpha1.KafkaTopic) error {
	reqLogger.Info("Addinag Finalizer for the KafkaTopic")
	topic.SetFinalizers(append(topic.GetFinalizers(), topicFinalizer))

	// Update CR
	err := r.Client.Update(context.TODO(), topic)
	if err != nil {
		reqLogger.Error(err, "Failed to update KafkaTopic with finalizer")
		return err
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	for i, v := range list {
		if v == s {
			list = append(list[:i], list[i+1:]...)
		}
	}
	return list
}
