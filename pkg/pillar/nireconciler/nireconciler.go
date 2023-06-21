// Copyright (c) 2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package nireconciler (Network Instance (config) Reconciler) is used by zedrouter
// to configure network instances and connect them with applications inside
// the target network stack.
// The main entry point is the interface of NIReconciler, which is expected
// to eventually have multiple implementations, one for every supported network
// stack (currently EVE only provides one implementation of network instances,
// built on top of the Linux bridge).
package nireconciler

import (
	"context"
	"net"
	"sort"

	dg "github.com/lf-edge/eve/libs/depgraph"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/lf-edge/eve/pkg/pillar/utils"
	uuid "github.com/satori/go.uuid"
)

// LogAndErrPrefix is prepended to every log message and error returned by NI Reconciler
// so that they are easy to filter in log file.
const LogAndErrPrefix = "NI Reconciler"

// NIReconciler translates the currently submitted config for network instances
// and application interfaces into the corresponding low-level network configuration
// of the target network stack and applies it using the Reconciler (see libs/reconciler).
type NIReconciler interface {
	// RunInitialReconcile is called once by zedrouter at startup before any NI
	// or Application connection is created.
	// It is expected to apply the initial configuration of the network stack.
	RunInitialReconcile(ctx context.Context)

	// ResumeReconcile should be called whenever ReconcilerUpdate of UpdateType
	// AsyncOpDone or CurrentStateChanged is received from the reconciler
	// (via channel returned by WatchReconcilerUpdates).
	ResumeReconcile(ctx context.Context)

	// ApplyUpdatedGCP : apply change in the global config properties.
	ApplyUpdatedGCP(ctx context.Context, gcp types.ConfigItemValueMap)

	// AddNI : create this new network instance inside the network stack.
	AddNI(ctx context.Context, niConfig types.NetworkInstanceConfig, br NIBridge) (
		NIReconcileStatus, error)
	// UpdateNI : apply a change in the intended NI configuration inside the network stack.
	UpdateNI(ctx context.Context, niConfig types.NetworkInstanceConfig, br NIBridge) (
		NIReconcileStatus, error)
	// DelNI : remove network instance from the network stack.
	DelNI(ctx context.Context, niID uuid.UUID) (NIReconcileStatus, error)

	// ConnectApp : make necessary changes inside the network stack to connect a new
	// application into the desired set of network instance(s).
	// This is called by zedrouter before the guest VM is started, meaning that
	// some of the operations will be completed later from within ResumeReconcile() after
	// domainmgr starts the VM. Use WatchReconcilerUpdates to watch for updates.
	// appNum is a positive integer number (>0) allocated for the application by zedrouter.
	// It is unique among all applications deployed on the node.
	// This number is persisted and doesn't change across app config changes or node
	// reboots.
	ConnectApp(ctx context.Context, appNetConfig types.AppNetworkConfig, appNum int,
		vifs []AppVIF, hvTypeKube bool) (AppConnReconcileStatus, error)
	// ReconnectApp : (re)connect application with changed config into the (possibly
	// changed) desired set of network instance(s).
	ReconnectApp(ctx context.Context, appNetConfig types.AppNetworkConfig, vifs []AppVIF) (
		AppConnReconcileStatus, error)
	// DisconnectApp : disconnect (removed) application from network instance(s).
	DisconnectApp(ctx context.Context, app uuid.UUID) (AppConnReconcileStatus, error)

	// WatchReconcilerUpdates returns channel with updates about the reconciliation
	// status, which is provided separately for every network instance and connected
	// application.
	WatchReconcilerUpdates() <-chan ReconcilerUpdate
}

// NIBridge either references an already created bridge (by NIM) that Reconciler
// should use for switch (L2) NI with uplink connectivity, or it describes parameters
// of a bridge that Reconciler should create/update for air-gapped switch NI or for
// local (L3, NATed) NI.
type NIBridge struct {
	// NI : UUID of the network instance.
	NI uuid.UUID
	// BrNum : a positive integer number (>0) allocated for the bridge by zedrouter.
	// Unique across all NI bridges.
	// This number is persisted and doesn't change across app config changes or node
	// reboots.
	// Can be used by Reconciler to for example generate a unique bridge interface name.
	BrNum int
	// MACAddress : MAC address allocated for (or already assigned by NIM to) the bridge.
	MACAddress net.HardwareAddr
	// IPAddress : IP address allocated for the bridge itself (with network mask).
	// Used only with L3 network instances.
	// Reconciler is expected to assign this address to the bridge that it will create.
	IPAddress *net.IPNet
	// Uplink interface selected for this network instance.
	// Zero value if network instance is air-gapped.
	Uplink Uplink
}

// Uplink used by a network instance to provide external connectivity for applications.
type Uplink struct {
	LogicalLabel string
	IfName       string
	DNSServers   []net.IP
	NTPServers   []net.IP
}

// Equal compares two uplinks for equality.
func (u Uplink) Equal(u2 Uplink) bool {
	return u.LogicalLabel == u2.LogicalLabel &&
		u.IfName == u2.IfName &&
		utils.EqualSetsFn(u.DNSServers, u2.DNSServers, utils.EqualIPs) &&
		utils.EqualSetsFn(u.NTPServers, u2.NTPServers, utils.EqualIPs)
}

// AppVIF : describes interface created to connect application with network instance.
// This comes from zedrouter.
type AppVIF struct {
	// App : application UUID.
	App uuid.UUID
	// NI : UUID of the network instance to which the application is connected through
	// this virtual interface.
	NI uuid.UUID
	// NetAdapterName is the logical name for this interface received from the controller
	// in NetworkAdapter.Name.
	// Unique in the scope of the application.
	NetAdapterName string
	// VIFNum : a positive integer number (>0) allocated for the application virtual
	// interface by zedrouter.
	// This number is only unique in the scope of the app (AppVIF.App).
	// Can be used by Reconciler to for example generate a unique VIF interface name
	// (when combined with appNum).
	VIFNum int
	// GuestIfMAC : MAC address assigned to VIF on the guest side (inside the app).
	GuestIfMAC net.HardwareAddr
	// GuestIP : IP address assigned to VIF on the guest side (inside the app).
	GuestIP net.IP
	// pass vif name in kube mode
	VifIfName string
}

// UpdateType : type of the ReconcilerUpdate.
type UpdateType int

const (
	// AsyncOpDone is a signal for the zedrouter that one or more asynchronous operations
	// have finalized and therefore NIReconciler.ResumeReconcile() should be called
	// to process them.
	AsyncOpDone UpdateType = iota
	// CurrentStateChanged is a signal for the zedrouter informing that the Reconciler
	// detected a change in the current state (e.g. an uplink port appeared) and therefore
	// NIReconciler.ResumeReconcile() should be called to reconcile the current and
	// the intended states.
	CurrentStateChanged
	// NIReconcileStatusChanged signals that the reconciliation status for one of NIs
	// have changed. The new status is available in ReconcilerUpdate.NIStatus
	NIReconcileStatusChanged
	// AppConnReconcileStatusChanged signals that reconciliation status for one
	// of the connected applications have changed. The new status is available
	// in ReconcilerUpdate.AppConnStatus.
	AppConnReconcileStatusChanged
)

// ReconcilerUpdate is published by the Reconciler whenever there is a status update
// related to the process of NI/App-connectivity config reconciliation.
type ReconcilerUpdate struct {
	// UpdateType : determines the type of the update.
	// ReconcilerUpdate is basically a union and UpdateType determines
	// which of the attributes below is defined (if any).
	UpdateType UpdateType
	// NIStatus is provided if UpdateType is NIReconcileStatusChanged.
	NIStatus *NIReconcileStatus
	// AppConnStatus is provided if UpdateType is AppConnReconcileStatusChanged.
	AppConnStatus *AppConnReconcileStatus
}

// NIReconcileStatus : status of the config reconciliation related to a particular
// network instance.
type NIReconcileStatus struct {
	// NI : network instance UUID.
	NI uuid.UUID
	// Deleted is true if the network instance was unconfigured.
	Deleted bool
	// BrIfName : name of the bridge interface inside the network stack.
	BrIfName string
	// BrIfIndex : integer used as a handle for the bridge interface
	// inside the network stack.
	BrIfIndex int
	// AsyncInProgress is true if any async operations are in progress.
	AsyncInProgress bool
	// FailedItems : The set of configuration items currently in a failed state.
	FailedItems map[dg.ItemRef]error
}

// Equal compares two instances of NIReconcileStatus.
func (s NIReconcileStatus) Equal(s2 NIReconcileStatus) bool {
	if len(s.FailedItems) != len(s2.FailedItems) {
		return false
	}
	for itemRef, itemErr := range s.FailedItems {
		if itemErr2, ok := s2.FailedItems[itemRef]; !ok || itemErr != itemErr2 {
			return false
		}
	}
	return s.NI == s2.NI && s.Deleted == s2.Deleted &&
		s.BrIfName == s2.BrIfName &&
		s.AsyncInProgress == s2.AsyncInProgress
}

// AppConnReconcileStatus : status of the config reconciliation related to application
// connectivity.
type AppConnReconcileStatus struct {
	// App : application UUID.
	App uuid.UUID
	// Deleted is true if the application was unconfigured.
	Deleted bool
	// VIFs : the reconciliation status reported separately for each VIF.
	VIFs []AppVIFReconcileStatus
}

// Equal compares two instances of AppConnReconcileStatus.
func (s AppConnReconcileStatus) Equal(s2 AppConnReconcileStatus) bool {
	return s.App == s2.App && s.Deleted == s2.Deleted &&
		utils.EqualSetsFn(s.VIFs, s2.VIFs,
			func(v1, v2 AppVIFReconcileStatus) bool {
				return v1.Equal(v2)
			})
}

// SortVIFs sorts the VIFs by VIFNum.
// No need for pointer receiver since VIFs is a slice, hence passed as a pointer,
// plus sort.Slice does not need to change the slice size.
func (s AppConnReconcileStatus) SortVIFs() {
	sort.Slice(s.VIFs, func(i, j int) bool {
		return s.VIFs[i].VIFNum < s.VIFs[j].VIFNum
	})
}

// AppVIFReconcileStatus : status of the config reconciliation related to a particular
// application VIF.
type AppVIFReconcileStatus struct {
	// NetAdapterName can be used to match AppVIFReconcileStatus with the corresponding
	// AppVIF.
	NetAdapterName string
	// VIFNum can be used to match AppVIFReconcileStatus with the corresponding AppVIF.
	VIFNum int
	// HostIfName : name of the interface inside the network stack on the host-side.
	HostIfName string
	// Ready is true if the VIF interface is ready for use or if some configuration
	// operations are still in progress or pending.
	// VIF is typically created in cooperation with zedmanager + domainmgr, meaning
	// that NIReconciler may spend some time waiting for an action to be completed
	// by other microservices.
	Ready bool
	// True if any async operations are in progress.
	AsyncInProgress bool
	// FailedItems : The set of configuration items currently in a failed state.
	FailedItems map[dg.ItemRef]error
}

// Equal compares two instances of AppVIFReconcileStatus.
func (s AppVIFReconcileStatus) Equal(s2 AppVIFReconcileStatus) bool {
	if len(s.FailedItems) != len(s2.FailedItems) {
		return false
	}
	for itemRef, itemErr := range s.FailedItems {
		if itemErr2, ok := s2.FailedItems[itemRef]; !ok || itemErr != itemErr2 {
			return false
		}
	}
	return s.NetAdapterName == s2.NetAdapterName && s.VIFNum == s2.VIFNum &&
		s.HostIfName == s2.HostIfName && s.Ready == s2.Ready &&
		s.AsyncInProgress == s2.AsyncInProgress
}
