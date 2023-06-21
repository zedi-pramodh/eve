// Copyright (c) 2017-2023 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

// Zedrouter creates and manages network instances - a set of virtual switches
// providing connectivity and various network services for applications.
// The configuration for these network instances comes from the controller as part
// of EdgeDevConfig. However, this is first retrieved and parsed by zedagent
// and in pieces published to corresponding microservices using pubsub channels.
// For application-specific configuration there is an extra hop through zedmanager,
// which uses pubsub to orchestrate the flow of configuration (and state) data
// between microservices directly involved in application management: volumemgr,
// domainmgr and last but not least zedrouter.
// Zedrouter subscribes specifically for NetworkInstanceConfig (from zedagent)
// and AppNetworkConfig (from zedmanager). Based on the configuration,
// it creates network instances and in cooperation with domainmgr connects
// applications to them.
// Zedrouter also collects state data and metrics, such as interface counters,
// dynamic IP assignments, flow statistics, etc. The state data are periodically
// or on-change published to zedagent to be further delivered to the controller.
// Zedrouter, NIM and wwan are 3 microservices that collectively manage all network
// services for edge node and deployed applications.

package zedrouter

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lf-edge/eve/pkg/pillar/agentbase"
	"github.com/lf-edge/eve/pkg/pillar/agentlog"
	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/cipher"
	"github.com/lf-edge/eve/pkg/pillar/devicenetwork"
	"github.com/lf-edge/eve/pkg/pillar/flextimer"
	"github.com/lf-edge/eve/pkg/pillar/netmonitor"
	"github.com/lf-edge/eve/pkg/pillar/nireconciler"
	"github.com/lf-edge/eve/pkg/pillar/nistate"
	"github.com/lf-edge/eve/pkg/pillar/objtonum"
	"github.com/lf-edge/eve/pkg/pillar/pidfile"
	"github.com/lf-edge/eve/pkg/pillar/pubsub"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/lf-edge/eve/pkg/pillar/uplinkprober"
	"github.com/lf-edge/eve/pkg/pillar/utils"
	"github.com/lf-edge/eve/pkg/pillar/zedcloud"
	"github.com/sirupsen/logrus"
)

const (
	agentName  = "zedrouter"
	runDirname = "/run/zedrouter"
	// Time limits for event loop handlers
	errorTime   = 3 * time.Minute
	warningTime = 40 * time.Second
	// Publish 4X more often than zedagent publishes to controller
	// to reduce effect of quantization errors
	publishTickerDivider = 4
	// After 30 min of a flow not being touched, the publication will be removed.
	flowStaleSec int64 = 1800
)

// Version is set from Makefile
var Version = "No version specified"

// zedrouter creates and manages network instances - a set of virtual switches
// providing connectivity and various network services for applications.
type zedrouter struct {
	agentbase.AgentBase
	pubSub *pubsub.PubSub
	logger *logrus.Logger
	log    *base.LogObject
	runCtx context.Context

	// CLI options
	versionPtr        *bool
	enableArpSnooping bool // enable/disable switch NI arp snooping

	agentStartTime     time.Time
	receivedConfigTime time.Time
	triggerNumGC       bool // For appNum and bridgeNum

	deviceNetworkStatus *types.DeviceNetworkStatus
	subGlobalConfig     pubsub.Subscription
	gcInitialized       bool
	initReconcileDone   bool

	// Replaceable components
	// (different implementations for different network stacks)
	niStateCollector nistate.Collector
	networkMonitor   netmonitor.NetworkMonitor
	niReconciler     nireconciler.NIReconciler
	reachProber      uplinkprober.ReachabilityProber
	uplinkProber     *uplinkprober.UplinkProber

	// Number allocators
	appNumAllocator     *objtonum.Allocator
	bridgeNumAllocator  *objtonum.Allocator
	appIntfNumPublisher *objtonum.ObjNumPublisher
	appIntfNumAllocator map[string]*objtonum.Allocator // key: network instance UUID as string

	// Info published to application via metadata server
	subLocationInfo pubsub.Subscription
	subWwanStatus   pubsub.Subscription
	subWwanMetrics  pubsub.Subscription
	subDomainStatus pubsub.Subscription
	subEdgeNodeInfo pubsub.Subscription

	// To collect uplink info
	subDeviceNetworkStatus pubsub.Subscription

	// Configuration for Network Instances
	subNetworkInstanceConfig pubsub.Subscription

	// Metrics for all network interfaces
	pubNetworkMetrics pubsub.Publication

	// Status and metrics collected for Network Instances
	pubNetworkInstanceStatus  pubsub.Publication
	pubNetworkInstanceMetrics pubsub.Publication

	// Configuration for application interfaces
	subAppNetworkConfig   pubsub.Subscription
	subAppNetworkConfigAg pubsub.Subscription // From zedagent
	subAppInstanceConfig  pubsub.Subscription // From zedagent to cleanup appInstMetadata

	// Status of application interfaces
	pubAppNetworkStatus pubsub.Publication

	// State data, metrics and logs collected from application (from domUs)
	pubAppInstMetaData          pubsub.Publication
	pubAppContainerStats        pubsub.Publication
	appContainerStatsCollecting bool
	appContainerStatsMutex      sync.Mutex // to protect appContainerStatsCollecting
	appContainerStatsInterval   uint32
	appContainerLogger          *logrus.Logger

	// Agent metrics
	zedcloudMetrics    *zedcloud.AgentMetrics
	pubZedcloudMetrics pubsub.Publication
	cipherMetrics      *cipher.AgentMetrics
	pubCipherMetrics   pubsub.Publication

	// Flow recording
	pubAppFlowMonitor pubsub.Publication
	flowPublishMap    map[string]time.Time

	// Decryption of cloud-init user data
	pubCipherBlockStatus pubsub.Publication
	subControllerCert    pubsub.Subscription
	subEdgeNodeCert      pubsub.Subscription
	decryptCipherContext cipher.DecryptCipherContext

	// Ticker for periodic publishing of metrics
	metricInterval uint32 // In seconds
	publishTicker  *flextimer.FlexTickerHandle

	// Retry NI or app network config that zedrouter failed to apply
	retryTimer *time.Timer
	// kube cluster
	hvTypeKube bool
}

// AddAgentSpecificCLIFlags adds CLI options
func (z *zedrouter) AddAgentSpecificCLIFlags(flagSet *flag.FlagSet) {
	z.versionPtr = flagSet.Bool("v", false, "Version")
}

func Run(ps *pubsub.PubSub, logger *logrus.Logger, log *base.LogObject, args []string) int {
	zedrouter := zedrouter{
		pubSub: ps,
		logger: logger,
		log:    log,
	}
	agentbase.Init(&zedrouter, logger, log, agentName,
		agentbase.WithArguments(args))

	if *zedrouter.versionPtr {
		fmt.Printf("%s: %s\n", agentName, Version)
		return 0
	}

	if err := zedrouter.init(); err != nil {
		log.Fatal(err)
	}
	if err := zedrouter.run(context.Background()); err != nil {
		log.Fatal(err)
	}
	return 0
}

func (z *zedrouter) init() (err error) {
	z.agentStartTime = time.Now()
	z.appContainerLogger = agentlog.CustomLogInit(logrus.InfoLevel)
	z.flowPublishMap = make(map[string]time.Time)
	z.deviceNetworkStatus = &types.DeviceNetworkStatus{}

	z.zedcloudMetrics = zedcloud.NewAgentMetrics()
	z.cipherMetrics = cipher.NewAgentMetrics(agentName)

	z.hvTypeKube = base.IsHVTypeKube()

	gcp := *types.DefaultConfigItemValueMap()
	z.appContainerStatsInterval = gcp.GlobalValueInt(types.AppContainerStatsInterval)

	if err = z.ensureDir(runDirname); err != nil {
		return err
	}
	// Must be done before calling nistate.NewLinuxCollector.
	if err = z.ensureDir(devicenetwork.DnsmasqLeaseDir); err != nil {
		return err
	}

	if err = z.initPublications(); err != nil {
		return err
	}
	if err = z.initSubscriptions(); err != nil {
		return err
	}

	z.decryptCipherContext.Log = z.log
	z.decryptCipherContext.AgentName = agentName
	z.decryptCipherContext.AgentMetrics = z.cipherMetrics
	z.decryptCipherContext.SubControllerCert = z.subControllerCert
	z.decryptCipherContext.SubEdgeNodeCert = z.subEdgeNodeCert

	// Initialize Zedrouter components (for Linux network stack).
	z.networkMonitor = &netmonitor.LinuxNetworkMonitor{Log: z.log}
	z.niStateCollector = nistate.NewLinuxCollector(z.log)
	controllerReachProber := uplinkprober.NewControllerReachProber(
		z.log, agentName, z.zedcloudMetrics)
	z.reachProber = controllerReachProber
	z.niReconciler = nireconciler.NewLinuxNIReconciler(z.log, z.logger, z.networkMonitor,
		z.makeMetadataHandler(), true, true)

	z.initNumberAllocators()
	return nil
}

func (z *zedrouter) run(ctx context.Context) (err error) {
	z.runCtx = ctx
	if err = pidfile.CheckAndCreatePidfile(z.log, agentName); err != nil {
		return err
	}
	z.log.Noticef("Starting %s", agentName)

	// Wait for initial GlobalConfig.
	if err = z.subGlobalConfig.Activate(); err != nil {
		return err
	}
	for !z.gcInitialized {
		z.log.Noticef("Waiting for GCInitialized")
		select {
		case change := <-z.subGlobalConfig.MsgChan():
			z.subGlobalConfig.ProcessChange(change)
		}
	}
	z.log.Noticef("Processed GlobalConfig")

	// Wait until we have been onboarded aka know our own UUID
	// (even though zedrouter does not use the UUID).
	err = utils.WaitForOnboarded(z.pubSub, z.log, agentName, warningTime, errorTime)
	if err != nil {
		return err
	}
	z.log.Noticef("Received device UUID")

	// Run a periodic timer so we always update StillRunning
	stillRunning := time.NewTicker(25 * time.Second)
	z.pubSub.StillRunning(agentName, warningTime, errorTime)

	// Timer used to retry failed configuration
	z.retryTimer = time.NewTimer(1 * time.Second)
	z.retryTimer.Stop()

	// Publish network metrics (interface counters, etc.)
	interval := time.Duration(z.metricInterval) * time.Second
	max := float64(interval) / publishTickerDivider
	min := max * 0.3
	publishTicker := flextimer.NewRangeTicker(time.Duration(min),
		time.Duration(max))
	z.publishTicker = &publishTicker

	// Start watchers
	reconcilerUpdates := z.niReconciler.WatchReconcilerUpdates()
	flowUpdates := z.niStateCollector.WatchFlows()
	ipAssignUpdates := z.niStateCollector.WatchIPAssignments()
	z.uplinkProber = uplinkprober.NewUplinkProber(
		z.log, uplinkprober.DefaultConfig(), z.reachProber)
	probeUpdates := z.uplinkProber.WatchProbeUpdates()

	// Activate all subscriptions.
	inactiveSubs := []pubsub.Subscription{
		z.subDeviceNetworkStatus,
		z.subEdgeNodeInfo,
		z.subControllerCert,
		z.subEdgeNodeCert,
		z.subNetworkInstanceConfig,
		z.subAppNetworkConfig,
		z.subAppNetworkConfigAg,
		z.subAppInstanceConfig,
		z.subLocationInfo,
		z.subWwanStatus,
		z.subWwanMetrics,
		z.subDomainStatus,
	}
	for _, sub := range inactiveSubs {
		if err = sub.Activate(); err != nil {
			return err
		}
	}

	z.log.Noticef("Entering main event loop")
	for {
		select {
		case change := <-z.subControllerCert.MsgChan():
			z.subControllerCert.ProcessChange(change)

		case change := <-z.subEdgeNodeCert.MsgChan():
			z.subEdgeNodeCert.ProcessChange(change)

		case change := <-z.subGlobalConfig.MsgChan():
			z.subGlobalConfig.ProcessChange(change)

		case change := <-z.subAppNetworkConfig.MsgChan():
			// If we have NetworkInstanceConfig process it first
			z.checkAndProcessNetworkInstanceConfig()
			z.subAppNetworkConfig.ProcessChange(change)

		case change := <-z.subAppNetworkConfigAg.MsgChan():
			z.subAppNetworkConfigAg.ProcessChange(change)

		case change := <-z.subAppInstanceConfig.MsgChan():
			z.subAppInstanceConfig.ProcessChange(change)

		case change := <-z.subDeviceNetworkStatus.MsgChan():
			z.subDeviceNetworkStatus.ProcessChange(change)

		case change := <-z.subLocationInfo.MsgChan():
			z.subLocationInfo.ProcessChange(change)

		case change := <-z.subWwanStatus.MsgChan():
			z.subWwanStatus.ProcessChange(change)

		case change := <-z.subDomainStatus.MsgChan():
			z.subDomainStatus.ProcessChange(change)

		case change := <-z.subWwanMetrics.MsgChan():
			z.subWwanMetrics.ProcessChange(change)

		case change := <-z.subEdgeNodeInfo.MsgChan():
			z.subEdgeNodeInfo.ProcessChange(change)

		case change := <-z.subNetworkInstanceConfig.MsgChan():
			z.subNetworkInstanceConfig.ProcessChange(change)

		case <-z.publishTicker.C:
			start := time.Now()
			z.log.Traceln("publishTicker at", time.Now())
			nms, err := z.niStateCollector.GetNetworkMetrics()
			if err == nil {
				err = z.pubNetworkMetrics.Publish("global", nms)
				if err != nil {
					z.log.Errorf("Failed to publish network metrics: %v", err)
				}
				z.publishNetworkInstanceMetricsAll(&nms)
			} else {
				z.log.Error(err)
			}

			err = z.cipherMetrics.Publish(
				z.log, z.pubCipherMetrics, "global")
			if err != nil {
				z.log.Errorln(err)
			}
			err = z.zedcloudMetrics.Publish(
				z.log, z.pubZedcloudMetrics, "global")
			if err != nil {
				z.log.Errorln(err)
			}

			z.pubSub.CheckMaxTimeTopic(agentName, "publishMetrics", start,
				warningTime, errorTime)
			// Check and remove stale flowlog publications.
			z.checkFlowUnpublish()

		case recUpdate := <-reconcilerUpdates:
			switch recUpdate.UpdateType {
			case nireconciler.AsyncOpDone:
				z.niReconciler.ResumeReconcile(ctx)
			case nireconciler.CurrentStateChanged:
				z.niReconciler.ResumeReconcile(ctx)
			case nireconciler.NIReconcileStatusChanged:
				key := recUpdate.NIStatus.NI.String()
				niStatus := z.lookupNetworkInstanceStatus(key)
				niConfig := z.lookupNetworkInstanceConfig(key)
				changed := z.processNIReconcileStatus(*recUpdate.NIStatus, niStatus)
				if changed {
					z.publishNetworkInstanceStatus(niStatus)
				}
				if niConfig == nil && recUpdate.NIStatus.Deleted &&
					niStatus != nil && !niStatus.HasError() &&
					niStatus.ChangeInProgress == types.ChangeInProgressTypeNone {
					z.unpublishNetworkInstanceStatus(niStatus)
				}
			case nireconciler.AppConnReconcileStatusChanged:
				key := recUpdate.AppConnStatus.App.String()
				appNetStatus := z.lookupAppNetworkStatus(key)
				appNetConfig := z.lookupAppNetworkConfig(key)
				changed := z.processAppConnReconcileStatus(*recUpdate.AppConnStatus,
					appNetStatus)
				if changed {
					z.publishAppNetworkStatus(appNetStatus)
				}
				if appNetConfig == nil && recUpdate.AppConnStatus.Deleted &&
					appNetStatus != nil && !appNetStatus.HasError() &&
					!appNetStatus.Pending() {
					z.unpublishAppNetworkStatus(appNetStatus)
				}
			}

		case flowUpdate := <-flowUpdates:
			z.flowPublish(flowUpdate)

		case ipAssignUpdates := <-ipAssignUpdates:
			ipAssignUpdate(z, ipAssignUpdates)

		case updates := <-probeUpdates:
			start := time.Now()
			z.log.Tracef("ProbeUpdate at %v", time.Now())
			for _, probeUpdate := range updates {
				niKey := probeUpdate.NetworkInstance.String()
				status := z.lookupNetworkInstanceStatus(niKey)
				if status == nil {
					z.log.Errorf("Failed to get status for network instance %s", niKey)
					continue
				}
				config := z.lookupNetworkInstanceConfig(niKey)
				if config == nil {
					z.log.Errorf("Failed to get config for network instance %s", niKey)
					continue
				}
				z.doUpdateNIUplink(probeUpdate.SelectedUplinkLL, status, *config)
			}
			z.pubSub.CheckMaxTimeTopic(agentName, "probeUpdates", start,
				warningTime, errorTime)

		case <-z.retryTimer.C:
			start := time.Now()
			z.log.Tracef("retryTimer: at %v", time.Now())
			z.retryFailedAppNetworks()
			z.pubSub.CheckMaxTimeTopic(agentName, "scanAppNetworkStatus", start,
				warningTime, errorTime)

		case <-stillRunning.C:
		}
		z.pubSub.StillRunning(agentName, warningTime, errorTime)
		// Are we likely to have seen all of the initial config?
		if z.triggerNumGC &&
			time.Since(z.receivedConfigTime) > 5*time.Minute {
			start := time.Now()
			z.gcNumAllocators()
			z.triggerNumGC = false
			z.pubSub.CheckMaxTimeTopic(agentName, "allocatorGC", start,
				warningTime, errorTime)
		}
	}
}

// ipAssignUpdate
func ipAssignUpdate(z *zedrouter, ipAssignUpdates []nistate.VIFAddrsUpdate) {
	for _, ipAssignUpdate := range ipAssignUpdates {
		vif := ipAssignUpdate.Prev.VIF
		newAddrs := ipAssignUpdate.New
		mac := vif.GuestIfMAC.String()
		niKey := vif.NI.String()
		netStatus := z.lookupNetworkInstanceStatus(niKey)
		if netStatus == nil {
			z.log.Errorf("Failed to get status for network instance %s "+
				"(needed to update IPs assigned to VIF %s)",
				niKey, vif.NetAdapterName)
			continue
		}
		netStatus.IPAssignments[mac] = types.AssignedAddrs{
			IPv4Addr:  newAddrs.IPv4Addr,
			IPv6Addrs: newAddrs.IPv6Addrs,
		}
		z.publishNetworkInstanceStatus(netStatus)
		appKey := vif.App.String()
		appStatus := z.lookupAppNetworkStatus(appKey)
		if appStatus == nil {
			z.log.Errorf("Failed to get network status for app %s "+
				"(needed to update IPs assigned to VIF %s)",
				appKey, vif.NetAdapterName)
			continue
		}
		for i := range appStatus.UnderlayNetworkList {
			ulStatus := &appStatus.UnderlayNetworkList[i]
			if ulStatus.Name != vif.NetAdapterName {
				continue
			}
			z.recordAssignedIPsToULStatus(ulStatus, &newAddrs)
			break
		}
		z.publishAppNetworkStatus(appStatus)
	}
}

func (z *zedrouter) initPublications() (err error) {
	z.pubNetworkInstanceStatus, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.NetworkInstanceStatus{},
	})
	if err != nil {
		return err
	}

	z.pubAppInstMetaData, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName:  agentName,
		Persistent: true,
		TopicType:  types.AppInstMetaData{},
	})
	if err != nil {
		return err
	}

	z.pubAppNetworkStatus, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.AppNetworkStatus{},
	})
	if err != nil {
		return err
	}
	if err = z.pubAppNetworkStatus.ClearRestarted(); err != nil {
		return err
	}

	z.pubNetworkInstanceMetrics, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.NetworkInstanceMetrics{},
	})
	if err != nil {
		return err
	}

	z.pubAppFlowMonitor, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.IPFlow{},
	})
	if err != nil {
		return err
	}

	z.pubAppContainerStats, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.AppContainerMetrics{},
	})
	if err != nil {
		return err
	}

	z.pubNetworkMetrics, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.NetworkMetrics{},
	})
	if err != nil {
		log.Fatal(err)
	}

	z.pubCipherBlockStatus, err = z.pubSub.NewPublication(
		pubsub.PublicationOptions{
			AgentName: agentName,
			TopicType: types.CipherBlockStatus{},
		})
	if err != nil {
		return err
	}

	z.pubCipherMetrics, err = z.pubSub.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.CipherMetrics{},
	})
	if err != nil {
		return err
	}

	z.pubZedcloudMetrics, err = z.pubSub.NewPublication(
		pubsub.PublicationOptions{
			AgentName: agentName,
			TopicType: types.MetricsMap{},
		})
	if err != nil {
		return err
	}
	return nil
}

func (z *zedrouter) initSubscriptions() (err error) {
	z.subGlobalConfig, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:     "zedagent",
		MyAgentName:   agentName,
		TopicImpl:     types.ConfigItemValueMap{},
		Persistent:    true,
		Activate:      false,
		CreateHandler: z.handleGlobalConfigCreate,
		ModifyHandler: z.handleGlobalConfigModify,
		DeleteHandler: z.handleGlobalConfigDelete,
		WarningTime:   warningTime,
		ErrorTime:     errorTime,
	})
	if err != nil {
		return err
	}

	z.subDeviceNetworkStatus, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:     "nim",
		MyAgentName:   agentName,
		TopicImpl:     types.DeviceNetworkStatus{},
		Activate:      false,
		CreateHandler: z.handleDNSCreate,
		ModifyHandler: z.handleDNSModify,
		DeleteHandler: z.handleDNSDelete,
		WarningTime:   warningTime,
		ErrorTime:     errorTime,
	})
	if err != nil {
		return err
	}

	// Look for edge node info
	z.subEdgeNodeInfo, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:   "zedagent",
		MyAgentName: agentName,
		TopicImpl:   types.EdgeNodeInfo{},
		Persistent:  true,
		Activate:    false,
	})
	if err != nil {
		return err
	}

	// Look for controller certs which will be used for decryption
	z.subControllerCert, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:   "zedagent",
		MyAgentName: agentName,
		TopicImpl:   types.ControllerCert{},
		Activate:    false,
		WarningTime: warningTime,
		ErrorTime:   errorTime,
		Persistent:  true,
	})
	if err != nil {
		return err
	}

	// Look for edge node certs which will be used for decryption
	z.subEdgeNodeCert, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:   "tpmmgr",
		MyAgentName: agentName,
		TopicImpl:   types.EdgeNodeCert{},
		Activate:    false,
		Persistent:  true,
		WarningTime: warningTime,
		ErrorTime:   errorTime,
	})
	if err != nil {
		return err
	}

	z.subNetworkInstanceConfig, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:     "zedagent",
		MyAgentName:   agentName,
		TopicImpl:     types.NetworkInstanceConfig{},
		Activate:      false,
		CreateHandler: z.handleNetworkInstanceCreate,
		ModifyHandler: z.handleNetworkInstanceModify,
		DeleteHandler: z.handleNetworkInstanceDelete,
		WarningTime:   warningTime,
		ErrorTime:     errorTime,
	})
	if err != nil {
		return err
	}

	// Subscribe to AppNetworkConfig from zedkube
	// since the zedmanager is not publishing the appnetconfig
	advAgentName := "zedmanager"
	if z.hvTypeKube {
		advAgentName = "zedkube"
	}
	// Subscribe to AppNetworkConfig from zedmanager
	z.subAppNetworkConfig, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:      advAgentName,
		MyAgentName:    agentName,
		TopicImpl:      types.AppNetworkConfig{},
		Activate:       false,
		CreateHandler:  z.handleAppNetworkCreate,
		ModifyHandler:  z.handleAppNetworkModify,
		DeleteHandler:  z.handleAppNetworkDelete,
		RestartHandler: z.handleRestart,
		WarningTime:    warningTime,
		ErrorTime:      errorTime,
	})
	if err != nil {
		return err
	}

	// Subscribe to AppNetworkConfig from zedagent
	z.subAppNetworkConfigAg, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:     "zedagent",
		MyAgentName:   agentName,
		TopicImpl:     types.AppNetworkConfig{},
		Activate:      false,
		CreateHandler: z.handleAppNetworkCreate,
		ModifyHandler: z.handleAppNetworkModify,
		DeleteHandler: z.handleAppNetworkDelete,
		WarningTime:   warningTime,
		ErrorTime:     errorTime,
	})
	if err != nil {
		return err
	}

	// Subscribe to AppInstConfig from zedagent
	z.subAppInstanceConfig, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:     "zedagent",
		MyAgentName:   agentName,
		TopicImpl:     types.AppInstanceConfig{},
		Activate:      false,
		DeleteHandler: z.handleAppInstDelete,
		WarningTime:   warningTime,
		ErrorTime:     errorTime,
	})
	if err != nil {
		return err
	}

	// Look for geographic location reports
	z.subLocationInfo, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:   "nim",
		MyAgentName: agentName,
		TopicImpl:   types.WwanLocationInfo{},
		Activate:    false,
		WarningTime: warningTime,
		ErrorTime:   errorTime,
	})
	if err != nil {
		return err
	}

	// Look for cellular status
	z.subWwanStatus, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:   "nim",
		MyAgentName: agentName,
		TopicImpl:   types.WwanStatus{},
		Activate:    false,
		WarningTime: warningTime,
		ErrorTime:   errorTime,
	})
	if err != nil {
		return err
	}

	// Look for cellular metrics
	z.subWwanMetrics, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:   "nim",
		MyAgentName: agentName,
		TopicImpl:   types.WwanMetrics{},
		Activate:    false,
		WarningTime: warningTime,
		ErrorTime:   errorTime,
	})
	if err != nil {
		return err
	}

	z.subDomainStatus, err = z.pubSub.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:   "domainmgr",
		MyAgentName: agentName,
		TopicImpl:   types.DomainStatus{},
		Activate:    false,
		WarningTime: warningTime,
		ErrorTime:   errorTime,
	})
	if err != nil {
		return err
	}
	return nil
}

// This functions updates but does not publish NetworkInstanceStatus.
// niStatus can be nil.
func (z *zedrouter) processNIReconcileStatus(recStatus nireconciler.NIReconcileStatus,
	niStatus *types.NetworkInstanceStatus) (changed bool) {
	key := recStatus.NI.String()
	if niStatus == nil {
		if !recStatus.Deleted {
			z.log.Errorf("Received NIReconcileStatus for unknown NI %s", key)
		}
		return false
	}
	if niStatus.BridgeIfindex != recStatus.BrIfIndex {
		niStatus.BridgeIfindex = recStatus.BrIfIndex
		changed = true
	}
	if niStatus.BridgeName != recStatus.BrIfName {
		niStatus.BridgeName = recStatus.BrIfName
		changed = true
	}
	if !recStatus.AsyncInProgress {
		if niStatus.ChangeInProgress != types.ChangeInProgressTypeNone {
			niStatus.ChangeInProgress = types.ChangeInProgressTypeNone
			changed = true
		}
	}
	if len(recStatus.FailedItems) > 0 {
		var failedItems []string
		for itemRef, itemErr := range recStatus.FailedItems {
			failedItems = append(failedItems, fmt.Sprintf("%v (%v)", itemRef, itemErr))
		}
		err := fmt.Errorf("failed items: %s", strings.Join(failedItems, ";"))
		if niStatus.Error != err.Error() {
			niStatus.SetErrorNow(err.Error())
			changed = true
		}
	} else {
		if niStatus.HasError() {
			niStatus.ClearError()
			changed = true
		}
	}
	return changed
}

// Updates but does not publish AppNetworkStatus.
// appNetStatus can be nil.
func (z *zedrouter) processAppConnReconcileStatus(
	recStatus nireconciler.AppConnReconcileStatus,
	appNetStatus *types.AppNetworkStatus) (changed bool) {
	key := recStatus.App.String()
	if appNetStatus == nil {
		if !recStatus.Deleted {
			z.log.Errorf("Received AppConnReconcileStatus for unknown AppNetwork %s", key)
		}
		return false
	}
	var asyncInProgress bool
	var failedItems []string
	for _, vif := range recStatus.VIFs {
		asyncInProgress = asyncInProgress || vif.AsyncInProgress
		for itemRef, itemErr := range vif.FailedItems {
			failedItems = append(failedItems, fmt.Sprintf("%v (%v)", itemRef, itemErr))
		}
		for i := range appNetStatus.UnderlayNetworkList {
			ulStatus := &appNetStatus.UnderlayNetworkList[i]
			if ulStatus.Name != vif.NetAdapterName {
				continue
			}
			if ulStatus.Vif != vif.HostIfName {
				ulStatus.Vif = vif.HostIfName
				changed = true
			}
		}
	}
	if !asyncInProgress {
		if appNetStatus.Pending() {
			changed = true
		}
		appNetStatus.PendingAdd = false
		appNetStatus.PendingModify = false
		appNetStatus.PendingDelete = false
	}
	if len(failedItems) > 0 {
		err := fmt.Errorf("failed items: %s", strings.Join(failedItems, ";"))
		if appNetStatus.Error != err.Error() {
			appNetStatus.SetErrorNow(err.Error())
			changed = true
		}
	} else {
		if appNetStatus.HasError() {
			appNetStatus.ClearError()
			changed = true
		}
	}
	return changed
}

func (z *zedrouter) ensureDir(path string) error {
	if _, err := os.Stat(path); err != nil {
		z.log.Functionf("Create directory %s", runDirname)
		if err := os.Mkdir(path, 0755); err != nil {
			return err
		}
	} else {
		// dnsmasq needs to read as nobody
		if err := os.Chmod(path, 0755); err != nil {
			return err
		}
	}
	return nil
}

// If we have an NetworkInstanceConfig process it first
func (z *zedrouter) checkAndProcessNetworkInstanceConfig() {
	select {
	case change := <-z.subNetworkInstanceConfig.MsgChan():
		z.log.Functionf("Processing NetworkInstanceConfig before AppNetworkConfig")
		z.subNetworkInstanceConfig.ProcessChange(change)
	default:
		z.log.Functionf("NO NetworkInstanceConfig before AppNetworkConfig")
	}
}

// maybeScheduleRetry : if any AppNetwork is in failed state, schedule a retry
// of the failed operation.
func (z *zedrouter) maybeScheduleRetry() {
	pub := z.pubAppNetworkStatus
	items := pub.GetAll()
	for _, st := range items {
		status := st.(types.AppNetworkStatus)
		config := z.lookupAppNetworkConfig(status.Key())
		if config == nil || !config.Activate || !status.HasError() {
			continue
		}
		z.log.Functionf("maybeScheduleRetry: retryTimer set to 60 seconds")
		z.retryTimer = time.NewTimer(60 * time.Second)
	}
}

func (z *zedrouter) lookupNetworkInstanceConfig(key string) *types.NetworkInstanceConfig {
	sub := z.subNetworkInstanceConfig
	c, _ := sub.Get(key)
	if c == nil {
		return nil
	}
	config := c.(types.NetworkInstanceConfig)
	return &config
}

func (z *zedrouter) lookupNetworkInstanceStatus(key string) *types.NetworkInstanceStatus {
	pub := z.pubNetworkInstanceStatus
	st, _ := pub.Get(key)
	if st == nil {
		return nil
	}
	status := st.(types.NetworkInstanceStatus)
	return &status
}

func (z *zedrouter) lookupNetworkInstanceMetrics(key string) *types.NetworkInstanceMetrics {
	pub := z.pubNetworkInstanceMetrics
	st, _ := pub.Get(key)
	if st == nil {
		return nil
	}
	status := st.(types.NetworkInstanceMetrics)
	return &status
}

func (z *zedrouter) lookupNetworkInstanceStatusByAppIP(
	ip net.IP) *types.NetworkInstanceStatus {
	pub := z.pubNetworkInstanceStatus
	items := pub.GetAll()
	for _, st := range items {
		status := st.(types.NetworkInstanceStatus)
		for _, addrs := range status.IPAssignments {
			if ip.Equal(addrs.IPv4Addr) {
				return &status
			}
			for _, nip := range addrs.IPv6Addrs {
				if ip.Equal(nip) {
					return &status
				}
			}
		}
	}
	return nil
}

func (z *zedrouter) lookupAppNetworkConfig(key string) *types.AppNetworkConfig {
	sub := z.subAppNetworkConfig
	c, _ := sub.Get(key)
	if c == nil {
		sub = z.subAppNetworkConfigAg
		c, _ = sub.Get(key)
		if c == nil {
			z.log.Tracef("lookupAppNetworkConfig(%s) not found", key)
			return nil
		}
	}
	config := c.(types.AppNetworkConfig)
	return &config
}

func (z *zedrouter) lookupAppNetworkStatus(key string) *types.AppNetworkStatus {
	pub := z.pubAppNetworkStatus
	st, _ := pub.Get(key)
	if st == nil {
		return nil
	}
	status := st.(types.AppNetworkStatus)
	return &status
}

func (z *zedrouter) publishNetworkInstanceStatus(status *types.NetworkInstanceStatus) {
	pub := z.pubNetworkInstanceStatus
	err := pub.Publish(status.Key(), *status)
	if err != nil {
		z.log.Errorf("publishNetworkInstanceStatus failed: %v", err)
	}
}

func (z *zedrouter) unpublishNetworkInstanceStatus(status *types.NetworkInstanceStatus) {
	pub := z.pubNetworkInstanceStatus
	st, _ := pub.Get(status.Key())
	if st == nil {
		return
	}
	err := pub.Unpublish(status.Key())
	if err != nil {
		z.log.Errorf("unpublishNetworkInstanceStatus failed: %v", err)
	}
}

func (z *zedrouter) publishAppNetworkStatus(status *types.AppNetworkStatus) {
	key := status.Key()
	pub := z.pubAppNetworkStatus
	err := pub.Publish(key, *status)
	if err != nil {
		z.log.Errorf("publishAppNetworkStatus failed: %v", err)
	}
}

func (z *zedrouter) unpublishAppNetworkStatus(status *types.AppNetworkStatus) {
	key := status.Key()
	pub := z.pubAppNetworkStatus
	st, _ := pub.Get(key)
	if st == nil {
		return
	}
	err := pub.Unpublish(key)
	if err != nil {
		z.log.Errorf("unpublishAppNetworkStatus failed: %v", err)
	}
}

func (z *zedrouter) flowPublish(flow types.IPFlow) {
	flowKey := flow.Key()
	z.flowPublishMap[flowKey] = time.Now()
	err := z.pubAppFlowMonitor.Publish(flowKey, flow)
	if err != nil {
		z.log.Errorf("flowPublish failed: %v", err)
	}
}

func (z *zedrouter) checkFlowUnpublish() {
	for k, m := range z.flowPublishMap {
		passed := int64(time.Since(m) / time.Second)
		if passed > flowStaleSec { // no update after 30 minutes, unpublish this flow
			z.log.Functionf("checkFlowUnpublish: key %s, sec passed %d, remove",
				k, passed)
			err := z.pubAppFlowMonitor.Unpublish(k)
			if err != nil {
				z.log.Errorf("checkFlowUnpublish failed: %v", err)
			}
			delete(z.flowPublishMap, k)
		}
	}
}

// this is periodic metrics handler
// nms must be the unmodified output from getNetworkMetrics()
func (z *zedrouter) publishNetworkInstanceMetricsAll(nms *types.NetworkMetrics) {
	pub := z.pubNetworkInstanceStatus
	niList := pub.GetAll()
	if niList == nil {
		return
	}
	for _, ni := range niList {
		status := ni.(types.NetworkInstanceStatus)
		netMetrics := z.createNetworkInstanceMetrics(&status, nms)
		err := z.pubNetworkInstanceMetrics.Publish(netMetrics.Key(), *netMetrics)
		if err != nil {
			z.log.Errorf("publishNetworkInstanceMetricsAll failed: %v", err)
		}
		z.publishNetworkInstanceStatus(&status)
	}
}

func (z *zedrouter) createNetworkInstanceMetrics(status *types.NetworkInstanceStatus,
	nms *types.NetworkMetrics) *types.NetworkInstanceMetrics {

	niMetrics := types.NetworkInstanceMetrics{
		UUIDandVersion: status.UUIDandVersion,
		DisplayName:    status.DisplayName,
		Type:           status.Type,
	}
	netMetrics := types.NetworkMetrics{}
	// Update status.VifMetricMap and get bridge metrics as a sum of all VIF metrics.
	brNetMetric := status.UpdateNetworkMetrics(z.log, nms)
	// Add metrics of the bridge interface itself into brNetMetric.
	status.UpdateBridgeMetrics(z.log, nms, brNetMetric)

	// XXX For some strange reason we do not include VIFs into the returned
	// NetworkInstanceMetrics.
	netMetrics.MetricList = []types.NetworkMetric{*brNetMetric}
	niMetrics.NetworkMetrics = netMetrics
	if status.WithUplinkProbing() {
		probeMetrics, err := z.uplinkProber.GetProbeMetrics(status.UUID)
		if err == nil {
			niMetrics.ProbeMetrics = probeMetrics
		} else {
			z.log.Error(err)
		}
	}

	niMetrics.VlanMetrics.NumTrunkPorts = status.NumTrunkPorts
	niMetrics.VlanMetrics.VlanCounts = status.VlanMap
	return &niMetrics
}

func (z *zedrouter) deleteNetworkInstanceMetrics(key string) {
	pub := z.pubNetworkInstanceMetrics
	if metrics := z.lookupNetworkInstanceMetrics(key); metrics != nil {
		err := pub.Unpublish(metrics.Key())
		if err != nil {
			z.log.Errorf("deleteNetworkInstanceMetrics failed: %v", err)
		}
	}
}

func (z *zedrouter) lookupDiskStatusList(key string) []types.DiskStatus {
	st, err := z.subDomainStatus.Get(key)
	if err != nil || st == nil {
		z.log.Warnf("lookupDiskStatusList: could not find domain %s", key)
		return nil
	}
	domainStatus := st.(types.DomainStatus)
	return domainStatus.DiskStatusList
}

func (z *zedrouter) lookupAppNetworkStatusByAppIP(ip net.IP) *types.AppNetworkStatus {
	pub := z.pubAppNetworkStatus
	items := pub.GetAll()
	for _, st := range items {
		status := st.(types.AppNetworkStatus)
		for _, ulStatus := range status.UnderlayNetworkList {
			if ulStatus.AllocatedIPv4Addr.Equal(ip) {
				return &status
			}
		}
	}
	return nil
}

func (z *zedrouter) publishAppInstMetadata(appInstMetadata *types.AppInstMetaData) {
	if appInstMetadata == nil {
		z.log.Errorf("publishAppInstMetadata: nil appInst metadata")
		return
	}
	key := appInstMetadata.Key()
	pub := z.pubAppInstMetaData
	err := pub.Publish(key, *appInstMetadata)
	if err != nil {
		z.log.Errorf("publishAppInstMetadata failed: %v", err)
	}
}

func (z *zedrouter) unpublishAppInstMetadata(appInstMetadata *types.AppInstMetaData) {
	if appInstMetadata == nil {
		z.log.Errorf("unpublishAppInstMetadata: nil appInst metadata")
		return
	}
	key := appInstMetadata.Key()
	pub := z.pubAppInstMetaData
	if exists, _ := pub.Get(key); exists == nil {
		z.log.Errorf("unpublishAppInstMetadata: key %s not found", key)
		return
	}
	err := pub.Unpublish(key)
	if err != nil {
		z.log.Errorf("unpublishAppInstMetadata failed: %v", err)
	}
}

func (z *zedrouter) lookupAppInstMetadata(key string) *types.AppInstMetaData {
	pub := z.pubAppInstMetaData
	st, _ := pub.Get(key)
	if st == nil {
		z.log.Tracef("lookupAppInstMetadata: key %s not found", key)
		return nil
	}
	appInstMetadata := st.(types.AppInstMetaData)
	return &appInstMetadata
}

// getSSHPublicKeys : returns trusted SSH public keys
func (z *zedrouter) getSSHPublicKeys(dc *types.AppNetworkConfig) []string {
	// TBD: add ssh keys into cypher block
	return nil
}

// getCloudInitUserData : returns decrypted cloud-init user data
func (z *zedrouter) getCloudInitUserData(dc *types.AppNetworkConfig) (string, error) {
	if dc.CipherBlockStatus.IsCipher {
		status, decBlock, err := cipher.GetCipherCredentials(
			&z.decryptCipherContext, dc.CipherBlockStatus)
		if err != nil {
			_ = z.pubCipherBlockStatus.Publish(status.Key(), status)
		}
		if err != nil {
			z.log.Errorf("%s, AppNetworkConfig CipherBlock decryption unsuccessful, "+
				"falling back to cleartext: %v", dc.Key(), err)
			if dc.CloudInitUserData == nil {
				z.cipherMetrics.RecordFailure(z.log, types.MissingFallback)
				return decBlock.ProtectedUserData, fmt.Errorf(
					"AppNetworkConfig CipherBlock decryption"+
						"unsuccessful (%s); no fallback data", err)
			}
			decBlock.ProtectedUserData = *dc.CloudInitUserData
			// We assume IsCipher is only set when there was some
			// data, hence this is a fallback if there is some cleartext.
			if decBlock.ProtectedUserData != "" {
				z.cipherMetrics.RecordFailure(z.log, types.CleartextFallback)
			} else {
				z.cipherMetrics.RecordFailure(z.log, types.MissingFallback)
			}
			return decBlock.ProtectedUserData, nil
		}
		z.log.Functionf("%s, AppNetworkConfig CipherBlock decryption successful",
			dc.Key())
		return decBlock.ProtectedUserData, nil
	}
	z.log.Functionf("%s, AppNetworkConfig CipherBlock not present", dc.Key())
	decBlock := types.EncryptionBlock{}
	if dc.CloudInitUserData == nil {
		z.cipherMetrics.RecordFailure(z.log, types.NoCipher)
		return decBlock.ProtectedUserData, nil
	}
	decBlock.ProtectedUserData = *dc.CloudInitUserData
	if decBlock.ProtectedUserData != "" {
		z.cipherMetrics.RecordFailure(z.log, types.NoCipher)
	} else {
		z.cipherMetrics.RecordFailure(z.log, types.NoData)
	}
	return decBlock.ProtectedUserData, nil
}

func (z *zedrouter) getExternalIPForApp(remoteIP net.IP) (net.IP, int) {
	netstatus := z.lookupNetworkInstanceStatusByAppIP(remoteIP)
	if netstatus == nil {
		z.log.Errorf("getExternalIPForApp: No NetworkInstanceStatus for %v", remoteIP)
		return nil, http.StatusNotFound
	}
	if netstatus.SelectedUplinkIntfName == "" {
		z.log.Warnf("getExternalIPForApp: No SelectedUplinkIntfName for %v", remoteIP)
		// Nothing to report */
		return nil, http.StatusNoContent
	}
	ip, err := types.GetLocalAddrAnyNoLinkLocal(*z.deviceNetworkStatus,
		0, netstatus.SelectedUplinkIntfName)
	if err != nil {
		z.log.Errorf("getExternalIPForApp: No externalIP for %s: %s",
			remoteIP.String(), err)
		return nil, http.StatusNoContent
	}
	return ip, http.StatusOK
}
