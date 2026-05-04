// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build k

// Dynamic reconciler for the sriov-network-device-plugin ConfigMap.
//
// The plugin (deployed by pkg/kube/sriov/sriov-device-plugin.yaml) reads
// /etc/pcidp/config.json once at startup.  EVE knows which Physical Functions
// are configured for SR-IOV at runtime via the AssignableAdapters publication
// from domainmgr; this file projects that state into the ConfigMap and bounces
// the local device-plugin pod so it picks up the change.
//
// Why per-node bounce: each node's VF inventory is different, and the
// daemonset pod on each node mounts the same ConfigMap.  Restarting only the
// local pod limits disruption to this node; already-running VMIs are
// unaffected (kubelet allocations are sticky to pod lifecycle).

package zedkube

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lf-edge/eve/pkg/pillar/kubeapi"
	"github.com/lf-edge/eve/pkg/pillar/types"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	sriovDPNamespace     = "kube-system"
	sriovDPConfigMapName = "sriovdp-config"
	sriovDPConfigKey     = "config.json"
	sriovDPPodLabel      = "app=sriovdp"
	sriovResourcePrefix  = "eve.network"
)

// sriovSelectors is the subset of the upstream NetDeviceSelectors schema we use.
// Reference: https://github.com/k8snetworkplumbingwg/sriov-network-device-plugin
type sriovSelectors struct {
	Vendors []string `json:"vendors,omitempty"`
	Devices []string `json:"devices,omitempty"`
	Drivers []string `json:"drivers,omitempty"`
	PfNames []string `json:"pfNames,omitempty"`
}

type sriovPool struct {
	ResourceName   string         `json:"resourceName"`
	ResourcePrefix string         `json:"resourcePrefix"`
	Selectors      sriovSelectors `json:"selectors"`
}

type sriovResourceList struct {
	ResourceList []sriovPool `json:"resourceList"`
}

// reconcileSRIOVDevicePlugin rebuilds the sriov-network-device-plugin ConfigMap
// from the current AssignableAdapters publication and bounces the local
// device-plugin pod on a config change.
//
// Idempotent and cheap when nothing changes — it short-circuits if the JSON is
// byte-identical to what's already in the ConfigMap.
func (z *zedkube) reconcileSRIOVDevicePlugin(aa *types.AssignableAdapters) {
	if aa == nil {
		return
	}
	if z.config == nil {
		// At zedkube startup, kubeapi.GetKubeConfig() can fail if k3s hasn't
		// written /run/.kube/k3s/k3s.yaml yet — leaving z.config nil for the
		// remainder of the process unless we re-acquire here.  Without this
		// retry, the AA Create event that fires on subscription Activate would
		// silently no-op and the reconciler would never run again until the
		// next AA modify (which may not happen for hours).
		cfg, err := kubeapi.GetKubeConfig()
		if err != nil {
			log.Warnf("reconcileSRIOVDevicePlugin: kube config not yet "+
				"available: %v", err)
			return
		}
		z.config = cfg
		log.Noticef("reconcileSRIOVDevicePlugin: acquired kube config on retry")
	}
	clientset, err := kubernetes.NewForConfig(z.config)
	if err != nil {
		log.Errorf("reconcileSRIOVDevicePlugin: NewForConfig: %v", err)
		return
	}

	pools := buildSRIOVPools(aa)
	desired := sriovResourceList{ResourceList: pools}
	desiredJSON, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		log.Errorf("reconcileSRIOVDevicePlugin: marshal: %v", err)
		return
	}

	ctx := context.Background()
	cmIface := clientset.CoreV1().ConfigMaps(sriovDPNamespace)

	cm, err := cmIface.Get(ctx, sriovDPConfigMapName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// Cluster bootstrap may have run before our daemonset manifest landed.
		// Create the ConfigMap so the plugin will find it on first start.
		newCM := &k8sv1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sriovDPConfigMapName,
				Namespace: sriovDPNamespace,
			},
			Data: map[string]string{sriovDPConfigKey: string(desiredJSON)},
		}
		if _, err := cmIface.Create(ctx, newCM, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
			log.Errorf("reconcileSRIOVDevicePlugin: create ConfigMap: %v", err)
			return
		}
		log.Noticef("reconcileSRIOVDevicePlugin: created %s/%s with %d pool(s)",
			sriovDPNamespace, sriovDPConfigMapName, len(pools))
		return
	}
	if err != nil {
		log.Errorf("reconcileSRIOVDevicePlugin: get ConfigMap: %v", err)
		return
	}

	if cm.Data[sriovDPConfigKey] == string(desiredJSON) {
		log.Tracef("reconcileSRIOVDevicePlugin: ConfigMap already up to date (%d pool(s))",
			len(pools))
		return
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[sriovDPConfigKey] = string(desiredJSON)
	if _, err := cmIface.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		log.Errorf("reconcileSRIOVDevicePlugin: update ConfigMap: %v", err)
		return
	}
	log.Noticef("reconcileSRIOVDevicePlugin: updated %s/%s with %d pool(s); restarting local plugin pod",
		sriovDPNamespace, sriovDPConfigMapName, len(pools))

	if err := deleteLocalSriovDpPod(ctx, clientset, z.nodeName); err != nil {
		// Non-fatal: the plugin will pick up the change on its next natural restart.
		log.Warnf("reconcileSRIOVDevicePlugin: bounce local DP pod: %v", err)
	}

	// Garbage-collect NetworkAttachmentDefinitions whose backing pool no longer
	// exists.  ensureSRIOVNAD (in the kubevirt path) only creates/updates NADs;
	// without this sweep, removing a PF from the device model leaves orphan NADs
	// in eve-kube-app that point at a non-existent device-plugin pool.
	currentResources := make(map[string]bool, len(pools))
	for _, p := range pools {
		currentResources[p.ResourcePrefix+"/"+p.ResourceName] = true
	}
	if err := garbageCollectSRIOVNADs(ctx, currentResources); err != nil {
		log.Warnf("reconcileSRIOVDevicePlugin: NAD GC: %v", err)
	}
}

// garbageCollectSRIOVNADs deletes NetworkAttachmentDefinitions in the
// eve-kube-app namespace whose k8s.v1.cni.cncf.io/resourceName annotation
// references an SR-IOV pool that no longer appears in the current
// AssignableAdapters projection.
//
// We scope by the eve.network/ resource-prefix annotation so we only touch
// NADs that ensureSRIOVNAD created — third-party NADs in the same namespace
// are left alone.
func garbageCollectSRIOVNADs(ctx context.Context, currentResources map[string]bool) error {
	nadClient, err := kubeapi.GetNetClientSet()
	if err != nil {
		return fmt.Errorf("get NAD client: %w", err)
	}
	nads, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(
		kubeapi.EVEKubeNameSpace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list NADs: %w", err)
	}
	for _, nad := range nads.Items {
		resName := nad.Annotations["k8s.v1.cni.cncf.io/resourceName"]
		if !strings.HasPrefix(resName, sriovResourcePrefix+"/") {
			continue
		}
		if currentResources[resName] {
			continue
		}
		if err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(
			kubeapi.EVEKubeNameSpace).Delete(ctx, nad.Name, metav1.DeleteOptions{}); err != nil &&
			!errors.IsNotFound(err) {
			log.Warnf("garbageCollectSRIOVNADs: delete %s: %v", nad.Name, err)
			continue
		}
		log.Noticef("garbageCollectSRIOVNADs: deleted stale NAD %s (pool %s gone)",
			nad.Name, resName)
	}
	return nil
}

// buildSRIOVPools projects AssignableAdapters into one pool per SR-IOV PF that
// has VFs configured at the kernel level.  PFs without VFs are skipped — the
// device plugin would refuse to enumerate them anyway.
func buildSRIOVPools(aa *types.AssignableAdapters) []sriovPool {
	pools := make([]sriovPool, 0)

	for _, ib := range aa.IoBundleList {
		if ib.Type != types.IoNetEthPF {
			continue
		}
		if ib.PciLong == "" || ib.Ifname == "" {
			continue
		}
		if ib.Vfs.Count == 0 {
			// PF declared but VFs not yet created (or sriov_numvfs == 0).
			// Skip silently; we'll be re-invoked when domainmgr publishes
			// updated bundle state.
			continue
		}

		vendor, device, err := readVfVendorDevice(ib.PciLong)
		if err != nil {
			log.Warnf("buildSRIOVPools: PF %s (%s): can't read VF vendor/device: %v",
				ib.Ifname, ib.PciLong, err)
			continue
		}

		// Resource name must match what kubevirt.go's sriovResourceName() produces
		// so VMI specs and pool advertisement line up.
		pools = append(pools, sriovPool{
			ResourceName:   ib.Ifname + "_vfs",
			ResourcePrefix: sriovResourcePrefix,
			Selectors: sriovSelectors{
				Vendors: []string{vendor},
				Devices: []string{device},
				Drivers: []string{"vfio-pci"},
				PfNames: []string{ib.Ifname},
			},
		})
	}
	return pools
}

// readVfVendorDevice returns the vendor and device IDs of the first VF derived
// from the given PF, formatted as 4-char lowercase hex (no "0x" prefix).
//
// The PF's own vendor:device is not what the device plugin needs — VFs have
// their own IDs (e.g. I350 PF 8086:1521, VF 8086:1520).  We read sysfs of the
// first virtual function (virtfn0) which kernel-creates as soon as
// sriov_numvfs is non-zero.
func readVfVendorDevice(pfBDF string) (vendor, device string, err error) {
	pfDir := filepath.Join("/sys/bus/pci/devices", pfBDF)
	vf0Link, err := os.Readlink(filepath.Join(pfDir, "virtfn0"))
	if err != nil {
		return "", "", fmt.Errorf("readlink virtfn0: %w", err)
	}
	vfDir := filepath.Join(pfDir, vf0Link)

	vRaw, err := os.ReadFile(filepath.Join(vfDir, "vendor"))
	if err != nil {
		return "", "", fmt.Errorf("read vendor: %w", err)
	}
	dRaw, err := os.ReadFile(filepath.Join(vfDir, "device"))
	if err != nil {
		return "", "", fmt.Errorf("read device: %w", err)
	}
	// sysfs files contain "0x8086\n" — strip prefix and whitespace.
	v := strings.TrimPrefix(strings.TrimSpace(string(vRaw)), "0x")
	d := strings.TrimPrefix(strings.TrimSpace(string(dRaw)), "0x")
	if v == "" || d == "" {
		return "", "", fmt.Errorf("empty vendor or device in sysfs (%q/%q)", v, d)
	}
	return v, d, nil
}

// deleteLocalSriovDpPod deletes the sriov-network-device-plugin pod scheduled
// on this node.  The DaemonSet recreates it within seconds; the new pod reads
// the updated ConfigMap on startup.
//
// We restrict the LIST by spec.nodeName so multi-node clusters only restart
// the local pod — neighbours' inventories are independent.
func deleteLocalSriovDpPod(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName is empty")
	}
	pods, err := clientset.CoreV1().Pods(sriovDPNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: sriovDPPodLabel,
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no sriovdp pod on node %s (label %s)", nodeName, sriovDPPodLabel)
	}
	for _, p := range pods.Items {
		if err := clientset.CoreV1().Pods(sriovDPNamespace).Delete(
			ctx, p.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", p.Name, err)
		}
		log.Noticef("deleteLocalSriovDpPod: deleted %s on node %s", p.Name, nodeName)
	}
	return nil
}

// AssignableAdapters pubsub handlers.  All three funnel into a single reconcile
// — the publication is keyed on "global" so create/modify look identical from
// our perspective, and a delete shouldn't happen in practice (domainmgr owns it
// for the device's lifetime) but is harmless.

func handleAssignableAdaptersCreate(ctxArg interface{}, _ string, statusArg interface{}) {
	z := ctxArg.(*zedkube)
	aa := statusArg.(types.AssignableAdapters)
	z.reconcileSRIOVDevicePlugin(&aa)
}

func handleAssignableAdaptersModify(ctxArg interface{}, _ string, statusArg interface{}, _ interface{}) {
	z := ctxArg.(*zedkube)
	aa := statusArg.(types.AssignableAdapters)
	z.reconcileSRIOVDevicePlugin(&aa)
}

func handleAssignableAdaptersDelete(ctxArg interface{}, _ string, _ interface{}) {
	z := ctxArg.(*zedkube)
	// Empty AA — clears all pools, which is the right thing if domainmgr
	// withdrew the publication entirely.
	empty := &types.AssignableAdapters{}
	z.reconcileSRIOVDevicePlugin(empty)
}
