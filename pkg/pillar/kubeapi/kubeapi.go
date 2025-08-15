// Copyright (c) 2024 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build kubevirt

package kubeapi

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	netclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/pubsub"
	lhv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"kubevirt.io/client-go/kubecli"
)

const (
	// EVEKubeNameSpace : Kubernetes namespace used to deploy VMIs/Pods running
	// user applications.
	EVEKubeNameSpace = "eve-kube-app"
	// EVEkubeConfigFile : K3s config file path.
	EVEkubeConfigFile = "/run/.kube/k3s/k3s.yaml"
	// NetworkInstanceNAD : name of a (singleton) NAD used to define connection between
	// pod and (any) network instance.
	NetworkInstanceNAD = "network-instance-attachment"
	// VolumeCSIClusterStorageClass : CSI clustered storage class
	VolumeCSIClusterStorageClass = "longhorn"
	// VolumeCSILocalStorageClass : default local storage class
	VolumeCSILocalStorageClass = "local-path"
	// KubevirtPodsRunning : Wait for node to be ready, and require kubevirt namespace have at least 4 pods running
	// (virt-api, virt-controller, virt-handler, and virt-operator)
	KubevirtPodsRunning = 4
	// EdgenodeClusterConfig file
	EdgeNodeClusterConfigFile = "/persist/status/zedagent/EdgeNodeClusterConfig/global.json"
)

const (
	errorTime   = 3 * time.Minute
	warningTime = 40 * time.Second
)

// GetKubeConfig : Get handle to Kubernetes config
func GetKubeConfig() (*rest.Config, error) {
	// Build the configuration from the kubeconfig file
	config, err := clientcmd.BuildConfigFromFlags("", EVEkubeConfigFile)
	if err != nil {
		return nil, err
	}
	return config, nil
}

// GetClientSet : Get handle to kubernetes clientset
func GetClientSet() (*kubernetes.Clientset, error) {

	// Build the configuration from the provided kubeconfig file
	config, err := GetKubeConfig()
	if err != nil {
		return nil, err
	}

	// Create the Kubernetes clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

// GetNetClientSet : Get handle to kubernetes netclientset
func GetNetClientSet() (*netclientset.Clientset, error) {

	// Build the configuration from the provided kubeconfig file
	config, err := GetKubeConfig()
	if err != nil {
		return nil, err
	}

	// Create the Kubernetes netclientset
	nclientset, err := netclientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return nclientset, nil
}

/* NOTE: This code is commented out instead of deleting, just to keep a reference in case
 * we decide to move back to using k8s API.
 *
// GetKubevirtClientSet : Get handle to kubernetes kubevirt clientset
func GetKubevirtClientSet(kubeconfig *rest.Config) (KubevirtClientset, error) {

	if kubeconfig == nil {
		c, err := GetKubeConfig()
		if err != nil {
			return nil, err
		}
		kubeconfig = c
	}

	config := *kubeconfig
	config.ContentConfig.GroupVersion = &kubevirtapi.GroupVersion
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	config.UserAgent = rest.DefaultKubernetesUserAgent()

	coreClient, err := kubernetes.NewForConfig(&config)
	if err != nil {
		return nil, err
	}

	client, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}

	return &kubevirtClient{restClient: client, Clientset: coreClient}, nil
}
*/

// WaitForKubernetes : Wait until kubernetes server is ready
func WaitForKubernetes(agentName string, ps *pubsub.PubSub, stillRunning *time.Ticker,
	alsoWatch ...pubsub.ChannelWatch) (err error) {

	var watches []pubsub.ChannelWatch
	stillRunningWatch := pubsub.ChannelWatch{
		Chan: reflect.ValueOf(stillRunning.C),
		Callback: func(_ interface{}) (exit bool) {
			ps.StillRunning(agentName, warningTime, errorTime)
			return false
		},
	}
	watches = append(watches, stillRunningWatch)

	var config *rest.Config
	checkTicker := time.NewTicker(5 * time.Second)
	startTime := time.Now()
	const maxWaitTime = 10 * time.Minute
	watches = append(watches, pubsub.ChannelWatch{
		Chan: reflect.ValueOf(checkTicker.C),
		Callback: func(_ interface{}) (exit bool) {
			currentTime := time.Now()
			if currentTime.Sub(startTime) > maxWaitTime {
				err = fmt.Errorf("time exceeded 10 minutes")
				return true
			}
			if _, err := os.Stat(EVEkubeConfigFile); err == nil {
				config, err = GetKubeConfig()
				if err == nil {
					return true
				}
			}
			return false
		},
	})

	watches = append(watches, alsoWatch...)

	// wait until the Kubernetes server is started
	pubsub.MultiChannelWatch(watches)

	if err != nil {
		return err
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	devUUID, err := os.Hostname()
	if err != nil {
		return err
	}

	// Wait for the Kubernetes clientset to be ready, node ready and kubevirt pods in Running status
	readyCh := make(chan bool)
	go waitForNodeReady(client, readyCh, devUUID)

	watches = nil
	watches = append(watches, stillRunningWatch)
	watches = append(watches, pubsub.ChannelWatch{
		Chan: reflect.ValueOf(readyCh),
		Callback: func(_ interface{}) (exit bool) {
			return true
		},
	})
	watches = append(watches, alsoWatch...)
	pubsub.MultiChannelWatch(watches)
	return nil
}

func waitForLonghornReady(client *kubernetes.Clientset, hostname string) error {
	// Only wait for longhorn if we are not in base-k3s mode.
	if err := registrationAppliedToCluster(); err == nil {
		// In base k3s mode, pillar not deploying redundant storage
		return nil
	}

	// First we'll gate on the longhorn daemonsets existing
	lhDaemonsets, err := client.AppsV1().DaemonSets("longhorn-system").
		List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list longhorn daemonsets: %v", err)
	}
	// Keep a running table of which expected Daemonsets exist
	var lhExpectedDaemonsets = map[string]bool{
		"longhorn-manager":    false,
		"longhorn-csi-plugin": false,
		"engine-image":        false,
	}
	// Check if each daemonset is running and ready on this node
	for _, lhDaemonset := range lhDaemonsets.Items {
		lhDsName := lhDaemonset.GetName()
		for dsPrefix := range lhExpectedDaemonsets {
			if strings.HasPrefix(lhDsName, dsPrefix) {
				lhExpectedDaemonsets[dsPrefix] = true
			}
		}

		var labelSelectors []string
		for dsLabelK, dsLabelV := range lhDaemonset.Spec.Template.Labels {
			labelSelectors = append(labelSelectors, dsLabelK+"="+dsLabelV)
		}
		pods, err := client.CoreV1().Pods("longhorn-system").List(context.Background(), metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + hostname,
			LabelSelector: strings.Join(labelSelectors, ","),
		})
		if err != nil {
			return fmt.Errorf("unable to get daemonset pods on node: %v", err)
		}
		if len(pods.Items) != 1 {
			return fmt.Errorf("longhorn daemonset:%s missing on this node", lhDsName)
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != "Running" {
				return fmt.Errorf("daemonset:%s not running on node", lhDsName)
			}
			for _, podContainerState := range pod.Status.ContainerStatuses {
				if !podContainerState.Ready {
					return fmt.Errorf("daemonset:%s not ready on node", lhDsName)
				}
			}
		}
	}

	for dsPrefix, dsPrefixExists := range lhExpectedDaemonsets {
		if !dsPrefixExists {
			return fmt.Errorf("longhorn missing daemonset:%s", dsPrefix)
		}
	}

	return nil
}

func waitForNodeReady(client *kubernetes.Clientset, readyCh chan bool, devUUID string) {
	err := wait.PollImmediate(time.Second, time.Minute*20, func() (bool, error) {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			labelSelector := metav1.LabelSelector{
				MatchLabels: map[string]string{"node-uuid": devUUID}}
			options := metav1.ListOptions{
				LabelSelector: metav1.FormatLabelSelector(&labelSelector)}
			nodes, err := client.CoreV1().Nodes().List(context.Background(), options)
			if err != nil {
				return err
			}

			var hostname string
			for _, node := range nodes.Items {
				hostname = node.Name
				break
			}
			if hostname == "" {
				return fmt.Errorf("node not found by label uuid %s", devUUID)
			}

			// Only wait for kubevirt if we are not in base-k3s mode.
			if err := registrationAppliedToCluster(); err == nil {
				// In base k3s mode, pillar not deploying kubevirt VM app instances
				return nil
			}

			// get all pods from kubevirt, and check if they are all running
			pods, err := client.CoreV1().Pods("kubevirt").
				List(context.Background(), metav1.ListOptions{
					FieldSelector: "status.phase=Running",
				})
			if err != nil {
				return err
			}
			// Wait for kubevirt namespace to have at least 4 pods running
			// (virt-api, virt-controller, virt-handler, and virt-operator)
			// to consider kubevirt system is ready
			if len(pods.Items) < KubevirtPodsRunning {
				return fmt.Errorf("kubevirt running pods less than 4")
			}

			err = waitForLonghornReady(client, hostname)
			return err
		})

		if err == nil {
			return true, nil
		}

		return false, nil
	})

	if err != nil {
		readyCh <- false
	} else {
		readyCh <- true
	}
}

// WaitForPVCReady: Loop until PVC is ready for timeout
func WaitForPVCReady(pvcName string, log *base.LogObject) error {
	clientset, err := GetClientSet()
	if err != nil {
		log.Errorf("WaitForPVCReady failed to get clientset err %v", err)
		return err
	}

	i := 10
	var count int
	var err2 error
	for {
		pvcs, err := clientset.CoreV1().PersistentVolumeClaims(EVEKubeNameSpace).
			List(context.Background(), metav1.ListOptions{})
		if err != nil {
			log.Errorf("GetPVCInfo failed to list pvc info err %v", err)
			err2 = err
		} else {

			count = 0
			for _, pvc := range pvcs.Items {
				pvcObjName := pvc.ObjectMeta.Name
				if strings.Contains(pvcObjName, pvcName) {
					count++
					log.Noticef("WaitForPVCReady(%d): get pvc %s", count, pvcObjName)
				}
			}
			if count == 1 {
				return nil
			}
		}
		i--
		if i <= 0 {
			break
		}
		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("WaitForPVCReady: time expired count %d, err %v", count, err2)
}

// CleanupStaleVMIRs : delete all VMI replica sets on single node. Used by domainmgr on startup.
func CleanupStaleVMIRs() (int, error) {
	// Only wait for kubevirt if we are not in base-k3s mode.
	if err := registrationAppliedToCluster(); err == nil {
		// In base k3s mode, pillar not deploying kubevirt VM app instances
		return 0, nil
	}

	kubeconfig, err := GetKubeConfig()
	if err != nil {
		return 0, fmt.Errorf("couldn't get the Kube Config: %v", err)
	}

	virtClient, err := kubecli.GetKubevirtClientFromRESTConfig(kubeconfig)
	if err != nil {
		return 0, fmt.Errorf("couldn't get the Kube client Config: %v", err)
	}

	ctx := context.Background()

	// get a list of our VM replica sets
	vmrsList, err := virtClient.ReplicaSet(EVEKubeNameSpace).List(metav1.ListOptions{})

	if err != nil {
		return 0, fmt.Errorf("couldn't get the Kubevirt VMs: %v", err)
	}

	var count int
	for _, vmirs := range vmrsList.Items {

		if err := virtClient.ReplicaSet(EVEKubeNameSpace).Delete(vmirs.ObjectMeta.Name, &metav1.DeleteOptions{}); err != nil {
			return count, fmt.Errorf("delete vmirs error: %v", err)
		}
		count++
	}

	// Get list of native container pods replica sets

	podrsList, err := virtClient.AppsV1().ReplicaSets(EVEKubeNameSpace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return count, fmt.Errorf("couldn't get the pod replica sets: %v", err)
	}

	for _, podrs := range podrsList.Items {

		err := virtClient.AppsV1().ReplicaSets(EVEKubeNameSpace).Delete(ctx, podrs.ObjectMeta.Name, metav1.DeleteOptions{})
		if err != nil {
			return count, fmt.Errorf("delete podrs error: %v", err)
		}
		count++
	}

	return count, nil
}

// DetachOldWorkloadUnreachableNode : handle unresponsive/unreachable node
// - delete all control plane pods for kubevirt and longhorn-system
// - update kubevirt label and annotation placed on node for scheduling/VMI ready state.
func DetachOldWorkloadUnreachableNode(log *base.LogObject, kubernetesHostName string) {
	if log == nil {
		return
	}
	if kubernetesHostName == "" {
		log.Errorf("DetachOldWorkloadUnreachableNode: a kubernetesHostName is required!")
		return
	}

	config, err := GetKubeConfig()
	if err != nil {
		log.Errorf("DetachOldWorkloadUnreachableNode: can't get kubeconfig %v", err)
		return
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Errorf("DetachOldWorkloadUnreachableNode: can't get clientset %v", err)
		return
	}

	// 1. Is the node unreachable?
	node, err := clientset.CoreV1().Nodes().Get(context.Background(), kubernetesHostName, metav1.GetOptions{})
	if (err != nil) || (node == nil) {
		log.Errorf("DetachOldWorkloadUnreachableNode: can't get node:%s object err:%v", kubernetesHostName, err)
		return
	}
	unreachable := false
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if taint.Key != "node.kubernetes.io/unreachable" {
			continue
		}
		if time.Since(taint.TimeAdded.Time) < time.Millisecond*38 {
			log.Noticef("DetachOldWorkloadUnreachableNode: node:%s not unreachable long enough: %v", kubernetesHostName, taint.TimeAdded.Time)
			continue
		}
		unreachable = true
	}
	if !unreachable {
		log.Errorf("DetachOldWorkloadUnreachableNode: node:%s NOT declared unreachable long enough to force delete all kubevirt pods", kubernetesHostName)
		return
	}

	node.Labels["kubevirt.io/schedulable"] = "false"
	now := time.Now().UTC()
	heartbeatTs := now.Add(-2 * time.Minute).Format(time.RFC3339)
	node.Annotations["kubevirt.io/heartbeat"] = heartbeatTs
	log.Noticef("DetachOldWorkloadUnreachableNode node:%s marking kubevirt label unschedulable and heartbeat to:%s", kubernetesHostName, heartbeatTs)
	_, err = clientset.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("DetachOldWorkloadUnreachableNode node:%s unable to update kubevirt label and annotation err:%v", kubernetesHostName, err)
	}

	log.Noticef("DetachOldWorkloadUnreachableNode: node:%s IS declared unreachable long enough to force delete all kubevirt pods", kubernetesHostName)
	// 2. find all the kubevirt pods on this node
	pods, err := clientset.CoreV1().Pods("kubevirt").List(context.Background(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + kubernetesHostName,
	})
	if err != nil {
		log.Errorf("DetachOldWorkloadUnreachableNode: can't get node:%s object err:%v", kubernetesHostName, err)
		return
	}

	// 3. Delete them all !
	// https://kubevirt.io/user-guide/cluster_admin/unresponsive_nodes/#deleting-stuck-vmis-when-the-whole-node-is-unresponsive
	// The length of this list should be max 4 (depending on scheduling): virt-api, virt-controller, virt-handler, virt-operator
	for _, pod := range pods.Items {
		podName := pod.ObjectMeta.Name

		log.Noticef("DetachOldWorkloadUnreachableNode deleting kubevirt pod:%s", podName)

		gracePeriod := int64(0)
		propagationPolicy := metav1.DeletePropagationForeground
		err = clientset.CoreV1().Pods("kubevirt").Delete(context.Background(), podName,
			metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				PropagationPolicy:  &propagationPolicy,
			})
		if err != nil {
			log.Errorf("DetachOldWorkloadUnreachableNode Can't delete pod:%s err:%v", podName, err)
		}
	}

	log.Noticef("DetachOldWorkloadUnreachableNode: node:%s IS declared unreachable long enough to force delete all longhorn-system pods", kubernetesHostName)
	// 4. find all the kubevirt pods on this node
	pods, err = clientset.CoreV1().Pods("longhorn-system").List(context.Background(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + kubernetesHostName,
	})
	if err != nil {
		log.Errorf("DetachOldWorkloadUnreachableNode: can't get node:%s object err:%v", kubernetesHostName, err)
		return
	}

	// 5. Delete them all !
	for _, pod := range pods.Items {
		podName := pod.ObjectMeta.Name

		log.Noticef("DetachOldWorkloadUnreachableNode deleting longhorn-system pod:%s", podName)

		gracePeriod := int64(0)
		propagationPolicy := metav1.DeletePropagationForeground
		err = clientset.CoreV1().Pods("longhorn-system").Delete(context.Background(), podName,
			metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				PropagationPolicy:  &propagationPolicy,
			})
		if err != nil {
			log.Errorf("DetachOldWorkloadUnreachableNode Can't delete pod:%s err:%v", podName, err)
		}
	}
	return

}

func DetachUtilVmirsReplicaSet(log *base.LogObject, vmiRsName string, replicaCount int) error {
	config, err := GetKubeConfig()
	if err != nil {
		log.Errorf("DetachUtilVmirsReplicaSet: can't get kubeconfig %v", err)
		return err
	}

	kvClientset, err := kubecli.GetKubevirtClientFromRESTConfig(config)
	if err != nil {
		log.Errorf("DetachUtilVmirsReplicaSet couldn't get the Kube client Config: %v", err)
		return err
	}

	vmirs, err := kvClientset.ReplicaSet(EVEKubeNameSpace).Get(vmiRsName, metav1.GetOptions{})
	if err == nil {
		reps := int32(replicaCount)
		vmirs.Spec.Replicas = &reps
		_, err := kvClientset.ReplicaSet(EVEKubeNameSpace).Update(vmirs)
		if err != nil {
			log.Noticef("DetachUtilVmirsReplicaSet vmirs:%s scaled to %d err:%v", vmiRsName, replicaCount, err)
			return err
		}
	}
	log.Noticef("DetachUtilVmirsReplicaSet complete for vmirs:%s", vmiRsName)
	return nil
}

func DetachUtilVmirsReplicaReset(log *base.LogObject, vmiRsName string) (err error) {
	vmiRsResetMaxTries := 60
	vmiRsResetTry := 0
	// Retries to handle connection issues to virt-api
	for {
		vmiRsResetTry++
		if vmiRsResetTry > vmiRsResetMaxTries {
			log.Errorf("DetachOldWorkload vmirs scale reset timeout, breaking...")
			break
		}
		time.Sleep(time.Second * 1)
		err = DetachUtilVmirsReplicaSet(log, vmiRsName, 0)
		if err != nil {
			log.Errorf("DetachOldWorkload retrying scale:%s to 0 err:%v", vmiRsName, err)
			time.Sleep(time.Second * 2)
			continue
		}

		err = DetachUtilVmirsReplicaSet(log, vmiRsName, 1)
		if err != nil {
			log.Errorf("DetachOldWorkload retrying scale:%s to 1 err:%v", vmiRsName, err)
			time.Sleep(time.Second * 2)
			continue
		}
		log.Noticef("DetachOldWorkload vmirs:%s scale reset", vmiRsName)
		return nil
	}
	return
}

func GetVmiRsName(log *base.LogObject, appDomainName string) (string, error) {
	vmiName, err := GetVmiName(log, appDomainName)
	if err != nil {
		return "", err
	}

	//
	// Kubernetes and kubevirt api handles
	//
	config, err := GetKubeConfig()
	if err != nil {
		log.Errorf("GetVmiRsName: can't get kubeconfig %v", err)
		return "", err
	}
	kvClientset, err := kubecli.GetKubevirtClientFromRESTConfig(config)
	if err != nil {
		log.Errorf("GetVmiRsName couldn't get the Kube client Config: %v", err)
		return "", err
	}

	vmiRsName := ""
	vmi, err := kvClientset.VirtualMachineInstance(EVEKubeNameSpace).Get(context.Background(), vmiName, &metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if len(vmi.ObjectMeta.OwnerReferences) > 0 {
		vmiRsName = vmi.ObjectMeta.OwnerReferences[0].Name
	}
	return vmiRsName, nil
}

func GetVmiName(log *base.LogObject, appDomainName string) (string, error) {
	//
	// Setup Handles to kubernetes
	//
	config, err := GetKubeConfig()
	if err != nil {
		log.Errorf("GetVmiName: can't get kubeconfig %v", err)
		return "", err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Errorf("GetVmiName: can't get clientset %v", err)
		return "", err
	}

	//
	// First find the pod/virt-launcher...
	//
	vmiName := ""
	vlPods, err := clientset.CoreV1().Pods(EVEKubeNameSpace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "kubevirt.io=virt-launcher,App-Domain-Name=" + appDomainName,
	})
	if err != nil {
		return vmiName, err
	}
	if err != nil {
		log.Errorf("GetVmiName: can't list virt-launcher pods err:%v", err)
		return vmiName, err
	}
	for _, pod := range vlPods.Items {
		// Get VMI name, the vm.kubevirt.io/name label value
		val, lblExists := pod.ObjectMeta.Labels["vm.kubevirt.io/name"]
		if lblExists {
			return val, nil
		}
	}
	return vmiName, nil
}

// detachOldWorkload is used when EVE detects a node is no longer reachable and was running a VM app instance
// This function will find the storage attached to that workload and detach it so that the VM app instance
// can be started on a remaining ready node.
// Caller is required to detect the VM app instances which
func DetachOldWorkload(log *base.LogObject, failedNodeName string, appDomainName string) {
	detachStart := time.Now()

	//
	// Sanity Checks
	//

	if log == nil {
		return
	}
	if failedNodeName == "" {
		log.Errorf("DetachOldWorkload: a node name is required!")
		return
	}
	log.Noticef("DetachOldWorkload node:%s", failedNodeName)

	//
	// Setup handles
	//

	config, err := GetKubeConfig()
	if err != nil {
		log.Errorf("DetachOldWorkload: can't get kubeconfig %v", err)
		return
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Errorf("DetachOldWorkload: can't get clientset %v", err)
		return
	}

	//
	// 1. Determine list of virt-launcher pods on the node
	//
	// Get host name pod is on.  The Pod lookup MUST be completed with List() as a pod which has been terminating long enough
	// to the point of 'tombstone' may just return NotFound and no object
	virtLauncherPodName := ""
	vmiName := ""
	appDomainNameLbl := ""
	vmiRsName := ""
	var vlPod *corev1.Pod = nil
	vlPods, err := clientset.CoreV1().Pods(EVEKubeNameSpace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "kubevirt.io=virt-launcher,App-Domain-Name=" + appDomainName,
	})
	if err != nil {
		log.Errorf("DetachOldWorkload: can't list virt-launcher pods err:%v", err)
		return
	}
	for _, pod := range vlPods.Items {
		if pod.Spec.NodeName != failedNodeName {
			continue
		}
		val, lblExists := pod.ObjectMeta.Labels["vm.kubevirt.io/name"]
		if lblExists {
			vmiName = val
		}
		val, lblExists = pod.ObjectMeta.Labels["App-Domain-Name"]
		if lblExists {
			appDomainNameLbl = val
		}
		virtLauncherPodName = pod.ObjectMeta.Name
		log.Noticef("DetachOldWorkload found pod:%s vmi:%s on failedNode:%s", virtLauncherPodName, vmiName, failedNodeName)
		vlPod = &pod
		break
	}

	//
	// 2. Make sure the node is unreachable
	//
	node, err := clientset.CoreV1().Nodes().Get(context.Background(), failedNodeName, metav1.GetOptions{})
	if (err != nil) || (node == nil) {
		log.Errorf("DetachOldWorkload: can't get node:%s object err:%v", failedNodeName, err)
		return
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type != "Ready" {
			continue
		}
		// Found the ready condition
		if condition.Status == "True" {
			log.Errorf("DetachOldWorkload: returning due to node:%s health Ready", failedNodeName)
			return
		}

		if condition.Message != "Kubelet stopped posting node status." {
			log.Errorf("DetachOldWorkload: node:%s not reporting in", failedNodeName)
			return
		}
	}

	kvClientset, err := kubecli.GetKubevirtClientFromRESTConfig(config)
	if err != nil {
		log.Errorf("DetachOldWorkload couldn't get the Kube client Config: %v", err)
		return
	}

	//
	// 3. Need VMIRs Name to scale replicas
	//
	if vmiName != "" {
		vmi, err := kvClientset.VirtualMachineInstance(EVEKubeNameSpace).Get(context.Background(), vmiName, &metav1.GetOptions{})
		if err == nil {
			if len(vmi.ObjectMeta.OwnerReferences) > 0 {
				vmiRsName = vmi.ObjectMeta.OwnerReferences[0].Name
			}
		}
	}

	//
	// 3. Node is unreachable, Get virt-handler pod name on failed node (kubevirt control plane, not VM pod)
	//
	virtHandlerPodName := ""
	pods, err := clientset.CoreV1().Pods("kubevirt").List(context.Background(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + failedNodeName,
		LabelSelector: "kubevirt.io=virt-handler",
	})
	if err != nil {
		log.Errorf("DetachOldWorkload unable to get virt-handler pod on node: %v", err)
		return
	}
	if (pods != nil) && (len(pods.Items) == 1) {
		virtHandlerPodName = pods.Items[0].ObjectMeta.Name
	}

	// Get longhorn vol names in pod
	lhVolNames := []string{}
	if vlPod != nil {
		for _, vol := range vlPod.Spec.Volumes {
			if vol.PersistentVolumeClaim == nil {
				continue
			}
			pvcName := vol.PersistentVolumeClaim.ClaimName

			pvc, err := clientset.CoreV1().PersistentVolumeClaims(EVEKubeNameSpace).Get(context.Background(), pvcName, metav1.GetOptions{})
			if err != nil {
				log.Errorf("DetachOldWorkload Can't get failed pod:%s PVC:%s err:%v", virtLauncherPodName, pvcName, err)
				continue
			}
			if pvc.ObjectMeta.Annotations["volume.kubernetes.io/storage-provisioner"] != "driver.longhorn.io" {
				continue
			}

			lhVolName := pvc.Spec.VolumeName
			lhVolNames = append(lhVolNames, lhVolName)
		}
	}

	// Get longhorn-manager pod name on failed node
	lhMgrPodName := ""
	lhMgrPods, err := clientset.CoreV1().Pods("longhorn-system").List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=longhorn-manager",
		FieldSelector: "spec.nodeName=" + failedNodeName,
	})
	if (err != nil) || (lhMgrPods == nil) {
		log.Errorf("DetachOldWorkload Can't get failed longhorn-manager err:%v", err)
		return
	}
	if len(lhMgrPods.Items) == 1 {
		lhMgrPodName = lhMgrPods.Items[0].ObjectMeta.Name
	}

	// Get replicas for vol name
	var replicaNames []string
	var allReps []*lhv1beta2.ReplicaList
	for _, lhVolName := range lhVolNames {
		lhVolReps, err := LonghornReplicaList(failedNodeName, lhVolName)
		if err != nil {
			log.Errorf("DetachOldWorkload Can't get failed replicas err:%v", err)
			continue
		}
		allReps = append(allReps, lhVolReps)
		for _, replica := range lhVolReps.Items {
			replicaNames = append(replicaNames, replica.ObjectMeta.Name)
		}
	}

	//
	// Log Actions before taking them, one searchable log string
	//
	detachLogRecipe := "DetachOldWorkload Cluster Detach volume from VM pod:%s vmi:%s failedNode:%s lhMgrPod:%s virtHandlerPod:%s replicas:%s"
	log.Noticef(detachLogRecipe, virtLauncherPodName, vmiName, failedNodeName, lhMgrPodName, virtHandlerPodName, strings.Join(replicaNames, ","))

	//
	// Start Cleanup
	//
	log.Noticef("DetachOldWorkload DetachOldWorkloadUnreachableNode for host:%s", failedNodeName)
	DetachOldWorkloadUnreachableNode(log, failedNodeName)

	gracePeriod := int64(0)
	propagationPolicy := metav1.DeletePropagationForeground

	// Push the kubevirt control plane to schedule new pod.
	if vmiRsName != "" {
		DetachUtilVmirsReplicaReset(log, vmiRsName)
	}

	if virtLauncherPodName != "" {
		log.Noticef("DetachOldWorkload Deleting virt-launcher pod:%s", virtLauncherPodName)
		// Delete virt-launcher pod on failed node
		err = clientset.CoreV1().Pods(EVEKubeNameSpace).Delete(context.Background(), virtLauncherPodName,
			metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				PropagationPolicy:  &propagationPolicy,
			})
		if err != nil {
			log.Errorf("DetachOldWorkload Can't delete terminating virt-launcher pod:%s err:%v", virtLauncherPodName, err)
		}
	}

	// Delete vmi
	if vmiName != "" {
		log.Noticef("DetachOldWorkload Deleting vmi:%s", vmiName)
		// Attempt to clear the finalizer
		vmi, err := kvClientset.VirtualMachineInstance(EVEKubeNameSpace).Get(context.Background(), vmiName, &metav1.GetOptions{})
		if err == nil {
			if len(vmi.ObjectMeta.OwnerReferences) > 0 {
				vmiRsName = vmi.ObjectMeta.OwnerReferences[0].Name
			}
			vmi.ObjectMeta.Finalizers = []string{} // for delete speed
			_, err := kvClientset.VirtualMachineInstance(vmi.ObjectMeta.Namespace).Update(context.Background(), vmi)
			log.Noticef("DetachOldWorkload vmi:%s finalizer update result err:%v", vmiName, err)
		}

		err = kvClientset.VirtualMachineInstance(EVEKubeNameSpace).Delete(context.Background(), vmiName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				PropagationPolicy:  &propagationPolicy,
			})
		if err != nil {
			log.Errorf("DetachOldWorkload couldn't delete the Kubevirt VMI:%s for pod:%s err:%v", vmiName, virtLauncherPodName, err)
		}
	}

	// Delete virt-handler pod on that node
	if virtHandlerPodName != "" {
		log.Noticef("DetachOldWorkload Deleting kubevirt virt-handler pod:%s", virtHandlerPodName)
		err = clientset.CoreV1().Pods("kubevirt").Delete(context.Background(), virtHandlerPodName,
			metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				PropagationPolicy:  &propagationPolicy,
			})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				log.Errorf("DetachOldWorkload Can't delete virt-handler pod:%s err:%v", virtHandlerPodName, err)
			}
		}
	}

	// Delete longhorn-manager pod on failed node, only once if we're called for multiple VMIs
	if lhMgrPodName != "" {
		log.Noticef("DetachOldWorkload Deleting longhorn-system pod:%s", lhMgrPodName)
		err = clientset.CoreV1().Pods("longhorn-system").Delete(context.Background(), lhMgrPodName,
			metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				PropagationPolicy:  &propagationPolicy,
			})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				log.Errorf("DetachOldWorkload Can't delete failed longhorn-manager pod:%s err:%v", lhMgrPodName, err)
			}
		}
	}

	// Delete replica for PVC on failed node
	// TODO test without
	for _, repList := range allReps {
		for _, replica := range repList.Items {
			log.Noticef("DetachOldWorkload Deleting replica:%s", replica.ObjectMeta.Name)
			if err := longhornReplicaDelete(replica.ObjectMeta.Name); err != nil {
				log.Errorf("DetachOldWorkload Can't delete failed replica:%s err:%v", replica.ObjectMeta.Name, err)
			}
		}
	}

	newVAReportsAttached := false
	maxVAAttachTries := 10 //about 30sec
	currentVAAttachTry := 0
	for {
		currentVAAttachTry++

		log.Noticef("DetachOldWorkload verifying old VA gone, lh vol node IDs correct, and new VA attached try:%d", currentVAAttachTry)

		//
		// First try to force delete all VAs
		//
		time.Sleep(time.Second * 1)
		vaList, err := GetVolumeAttachmentFromHost(failedNodeName, log)
		if len(vaList) == 0 {
			log.Noticef("DetachOldWorkload node/%s volumeattachment list empty finally", failedNodeName)
			break
		}
		for _, va := range vaList {
			log.Noticef("DetachOldWorkload Deleting volumeattachment %s on remote node %s", va, failedNodeName)
			err = DeleteVolumeAttachment(va, log)
			if err != nil {
				log.Errorf("DetachOldWorkload Error deleting volumeattachment %s from PV %v", va, err)
				continue
			}
		}

		// Wait a moment for consistency
		time.Sleep(time.Second * 1)

		// Sometimes at this point we get an odd mismatch in the new node listed in the:
		// - virt-launcher pod
		// - volumeattachment
		// - lh vol spec.nodeID
		// - lh vol engine spec.nodeID
		if appDomainNameLbl == "" {
			appDomainNameLbl = appDomainName
		}
		newNodeName := ""
		// Find the new nodeID
		vlPods, err = clientset.CoreV1().Pods(EVEKubeNameSpace).List(context.Background(), metav1.ListOptions{
			LabelSelector: "kubevirt.io=virt-launcher,App-Domain-Name=" + appDomainNameLbl,
		})
		if err != nil {
			log.Errorf("DetachOldWorkload: can't list virt-launcher pods err:%v", err)
			return
		}
		for _, vlPod := range vlPods.Items {
			if vlPod.Spec.NodeName == failedNodeName {
				continue
			}

			//
			// Found newly scheduled virt-launcher pod
			//
			newNodeName = vlPods.Items[0].Spec.NodeName

			//
			// Get new ref to all its vols
			//
			for _, vol := range vlPods.Items[0].Spec.Volumes {
				if vol.PersistentVolumeClaim == nil {
					continue
				}
				pvcName := vol.PersistentVolumeClaim.ClaimName

				pvc, err := clientset.CoreV1().PersistentVolumeClaims(EVEKubeNameSpace).Get(context.Background(), pvcName, metav1.GetOptions{})
				if err != nil {
					log.Errorf("DetachOldWorkload Can't get failed pod:%s PVC:%s err:%v", virtLauncherPodName, pvcName, err)
					continue
				}
				if pvc.ObjectMeta.Annotations["volume.kubernetes.io/storage-provisioner"] != "driver.longhorn.io" {
					continue
				}

				lhVolName := pvc.Spec.VolumeName
				lhVolNames = append(lhVolNames, lhVolName)
			}
		}

		//
		// Force set new scheduled node id
		//
		for _, lhVolName := range lhVolNames {
			err = longhornVolumeSetNode(lhVolName, newNodeName)
			log.Noticef("DetachOldWorkload: set lhVol:%s to node:%s err:%v", lhVolName, newNodeName, err)

			attached, err := GetVolumeAttachmentAttached(lhVolName, newNodeName, log)
			log.Noticef("DetachOldWorkload: check attachment lhVol:%s node:%s attached:%v err:%v", lhVolName, newNodeName, attached, err)
			newVAReportsAttached = attached
		}

		if newVAReportsAttached {
			break
		}
		if currentVAAttachTry > maxVAAttachTries {
			// don't watchdog
			break
		}
	}
	// Retry above until va reports attached

	log.Noticef("DetachOldWorkload Completed failover for pod:%s duration:%v", virtLauncherPodName, time.Since(detachStart))
	return
}

// IsClusterMode : Returns true if this node is part of a cluster by checking EdgeNodeClusterConfigFile
func IsClusterMode() bool {

	fileInfo, err := os.Stat(EdgeNodeClusterConfigFile)
	if os.IsNotExist(err) {
		logrus.Infof("This node is not in cluster mode")
		return false
	} else if err != nil {
		logrus.Errorf("Error checking file '%s': %v", EdgeNodeClusterConfigFile, err)
		return false
	}

	if fileInfo.Size() > 0 {
		logrus.Infof("This node is in cluster mode")
		return true
	}

	return false
}
