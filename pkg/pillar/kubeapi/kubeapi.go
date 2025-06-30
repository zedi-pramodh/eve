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

// CleanupStaleVMI : delete all VMIs. Used by domainmgr on startup.
func CleanupStaleVMI() (int, error) {
	kubeconfig, err := GetKubeConfig()
	if err != nil {
		return 0, fmt.Errorf("couldn't get the Kube Config: %v", err)
	}

	clientset, err := kubecli.GetKubevirtClientFromRESTConfig(kubeconfig)
	if err != nil {
		return 0, fmt.Errorf("couldn't get the Kube client Config: %v", err)
	}

	ctx := context.Background()

	// get a list of our VMs
	vmiList, err := clientset.VirtualMachineInstance(EVEKubeNameSpace).List(ctx, &metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("couldn't get the Kubevirt VMs: %v", err)
	}

	var count int
	for _, vmi := range vmiList.Items {
		if err := clientset.VirtualMachineInstance(EVEKubeNameSpace).Delete(ctx, vmi.ObjectMeta.Name, &metav1.DeleteOptions{}); err != nil {
			return count, fmt.Errorf("delete vmi error: %v", err)
		}
		count++
	}
	return count, nil
}

// detachOldWorkload is used when EVE detects a node is no longer reachable and was running a VM app instance
// This function will find the storage attached to that workload and detach it so that the VM app instance
// can be started on a remaining ready node.
// Caller is required to detect the VM app instances which
func DetachOldWorkload(log *base.LogObject, virtLauncherPodName string) {
	if log == nil {
		return
	}
	if virtLauncherPodName == "" {
		log.Errorf("DetachOldWorkload: a virt-launcher pod name is required!")
	}

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
	// Collect info before we start cleanup
	//

	// Get host name pod is on
	vlPod, err := clientset.CoreV1().Pods(EVEKubeNameSpace).Get(context.Background(), virtLauncherPodName, metav1.GetOptions{})
	if (err != nil) || (vlPod == nil) {
		log.Errorf("DetachOldWorkload: can't get pod:%s object err:%v", virtLauncherPodName, err)
		return
	}
	kubernetesHostName := vlPod.Spec.NodeName

	// Get VMI name, the vm.kubevirt.io/name label value
	vmiName, vmiNameLabelExists := vlPod.ObjectMeta.Labels["vm.kubevirt.io/name"]
	if !vmiNameLabelExists {
		log.Errorf("DetachOldWorkload: virt-launcher pod:%s is missing vmi name label", virtLauncherPodName)
		return
	}

	// Ensure VMI is terminating
	kvClientset, err := kubecli.GetKubevirtClientFromRESTConfig(config)
	if err != nil {
		log.Errorf("DetachOldWorkload couldn't get the Kube client Config: %v", err)
		return
	}
	ctx := context.Background()

	vmiTerminatingWaitTry := 0
	maxVmiTerminatingWaitTries := 100
	for vmiTerminatingWaitTry < maxVmiTerminatingWaitTries {
		vmiTerminatingWaitTry++
		time.Sleep(30)
		// get a list of our VMs
		vmi, err := kvClientset.VirtualMachineInstance(EVEKubeNameSpace).Get(ctx, vmiName, &metav1.GetOptions{})
		if err != nil {
			log.Errorf("DetachOldWorkload couldn't get the Kubevirt VMI:%s for pod:%s err:%v", vmiName, virtLauncherPodName, err)
			continue
		}
		// Exit if not terminating
		if vmi.ObjectMeta.DeletionTimestamp == nil {
			log.Noticef("DetachOldWorkload Can't Detach yet, waiting for terminating vmi:%s try:%d", vmiName, vmiTerminatingWaitTry)
		}
		if vmi.ObjectMeta.DeletionTimestamp != nil {
			break
		}
	}

	// Get longhorn vol names in pod
	lhVolNames := []string{}
	for _, vol := range vlPod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		pvcName := vol.PersistentVolumeClaim.ClaimName

		pvc, err := clientset.CoreV1().PersistentVolumeClaims(EVEKubeNameSpace).Get(context.Background(), pvcName, metav1.GetOptions{})
		if err != nil {
			log.Errorf("DetachOldWorkload Can't get failed pod:%s PVC:%s err:%v", virtLauncherPodName, pvcName, err)
			return
		}
		if pvc.ObjectMeta.Annotations["volume.kubernetes.io/storage-provisioner"] != "driver.longhorn.io" {
			continue
		}

		lhVolName := pvc.Spec.VolumeName
		lhVolNames = append(lhVolNames, lhVolName)
	}

	// Get longhorn-manager pod name on failed node
	// "kubectl -n longhorn-system get pods -l app=longhorn-manager -o wide"
	lhMgrPods, err := clientset.CoreV1().Pods("longhorn-system").List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=longhorn-manager",
		FieldSelector: "spec.nodeName=" + kubernetesHostName,
	})
	if (err != nil) || (lhMgrPods == nil) {
		log.Errorf("DetachOldWorkload Can't get failed longhorn-manager err:%v", err)
		return
	}
	if len(lhMgrPods.Items) != 1 {
		log.Errorf("DetachOldWorkload Invalid number of longhorn-manager pods")
		return
	}
	lhMgrPodName := lhMgrPods.Items[0].ObjectMeta.Name

	// Get replicas for vol name
	var replicaNames []string
	var allReps []*lhv1beta2.ReplicaList
	for _, lhVolName := range lhVolNames {
		lhVolReps, err := LonghornReplicaList(kubernetesHostName, lhVolName)
		if err != nil {
			log.Errorf("DetachOldWorkload Can't get failed replicas err:%v", err)
			return
		}
		allReps = append(allReps, lhVolReps)
		for _, replica := range lhVolReps.Items {
			replicaNames = append(replicaNames, replica.ObjectMeta.Name)
		}
	}

	//
	// Log Actions before taking them, one searchable log string
	//
	detachLogRecipe := "DetachOldWorkload Cluster Detach volume from VM pod:%s on host:%s using replicas:%s"
	log.Noticef(detachLogRecipe, virtLauncherPodName, kubernetesHostName, strings.Join(replicaNames, ","))

	//
	// Start Cleanup
	//

	log.Noticef("DetachOldWorkload Deleting virt-launcher pod:%s", virtLauncherPodName)
	// Delete virt-launcher pod on failed node
	gracePeriod := int64(0)
	propagationPolicy := metav1.DeletePropagationBackground
	err = clientset.CoreV1().Pods(EVEKubeNameSpace).Delete(context.Background(), virtLauncherPodName,
		metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
			PropagationPolicy:  &propagationPolicy,
		})
	if err != nil {
		log.Errorf("DetachOldWorkload Can't delete terminating virt-launcher pod:%s err:%v", virtLauncherPodName, err)
		return
	}

	log.Noticef("DetachOldWorkload Deleting longhorn-system pod:%s", lhMgrPodName)
	// Delete longhorn-manager pod on failed node
	err = clientset.CoreV1().Pods("longhorn-system").Delete(context.Background(), lhMgrPodName,
		metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
			PropagationPolicy:  &propagationPolicy,
		})
	if err != nil {
		log.Errorf("DetachOldWorkload Can't delete failed longhorn-manager pod:%s err:%v", lhMgrPodName, err)
		return
	}

	// Delete replica for PVC on failed node
	for _, repList := range allReps {
		for _, replica := range repList.Items {
			log.Noticef("DetachOldWorkload Deleting replica:%s", replica.ObjectMeta.Name)
			if err := longhornReplicaDelete(replica.ObjectMeta.Name); err != nil {
				log.Errorf("DetachOldWorkload Can't delete failed replica:%s err:%v", replica.ObjectMeta.Name, err)
			}
		}
	}

	log.Noticef("DetachOldWorkload Completed failover for pod:%s", virtLauncherPodName)
	return
}
