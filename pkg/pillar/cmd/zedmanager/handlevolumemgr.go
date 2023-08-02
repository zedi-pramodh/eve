// Copyright (c) 2020 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package zedmanager

// Code for the interface with VolumeMgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/types"
	uuid "github.com/satori/go.uuid"
)

// MaybeAddVolumeRefConfig publishes volume ref config with refcount
// to the volumemgr
func MaybeAddVolumeRefConfig(ctx *zedmanagerContext, appInstID uuid.UUID,
	volumeID uuid.UUID, generationCounter, localGenerationCounter int64,
	mountDir string, verifyOnly bool) {

	var key string

	if base.IsHVTypeKube() {
		key = fmt.Sprintf("%s-pvc-%d", volumeID.String(),
			generationCounter+localGenerationCounter)
	} else {
		key = fmt.Sprintf("%s#%d", volumeID.String(),
			generationCounter+localGenerationCounter)
	}
	log.Functionf("MaybeAddVolumeRefConfig for %s", key)
	m := lookupVolumeRefConfig(ctx, key)
	if m != nil {
		m.RefCount++
		// only update from VerifyOnly to non-VerifyOnly
		if m.VerifyOnly {
			m.VerifyOnly = verifyOnly
		}
		log.Functionf("VolumeRefConfig exists for %s to refcount %d",
			key, m.RefCount)
		publishVolumeRefConfig(ctx, m)
	} else {
		log.Tracef("MaybeAddVolumeRefConfig: add for %s", key)
		vrc := types.VolumeRefConfig{
			VolumeID:               volumeID,
			GenerationCounter:      generationCounter,
			LocalGenerationCounter: localGenerationCounter,
			RefCount:               1,
			MountDir:               mountDir,
			VerifyOnly:             verifyOnly,
		}
		publishVolumeRefConfig(ctx, &vrc)
	}
	base.NewRelationObject(log, base.AddRelationType, base.AppInstanceConfigLogType, appInstID.String(),
		base.VolumeRefConfigLogType, key).Noticef("App instance to volume relation.")
	log.Functionf("MaybeAddVolumeRefConfig done for %s", key)
}

// MaybeRemoveVolumeRefConfig decreases the RefCount and deletes the VolumeRefConfig
// when the RefCount reaches zero
func MaybeRemoveVolumeRefConfig(ctx *zedmanagerContext, appInstID uuid.UUID,
	volumeID uuid.UUID, generationCounter, localGenerationCounter int64) {

	var key string

	if base.IsHVTypeKube() {
		key = fmt.Sprintf("%s-pvc-%d", volumeID.String(),
			generationCounter+localGenerationCounter)
	} else {
		key = fmt.Sprintf("%s#%d", volumeID.String(),
			generationCounter+localGenerationCounter)
	}

	log.Functionf("MaybeRemoveVolumeRefConfig for %s", key)
	m := lookupVolumeRefConfig(ctx, key)
	if m == nil {
		log.Functionf("MaybeRemoveVolumeRefConfig: config missing for %s", key)
		return
	}
	if m.RefCount == 0 {
		log.Fatalf("MaybeRemoveVolumeRefConfig: Attempting to reduce "+
			"0 RefCount for %s", key)
	}
	m.RefCount--
	if m.RefCount == 0 {
		log.Functionf("MaybeRemoveVolumeRefConfig deleting %s", key)
		unpublishVolumeRefConfig(ctx, key)
	} else {
		log.Functionf("MaybeRemoveVolumeRefConfig remaining RefCount %d for %s",
			m.RefCount, key)
		publishVolumeRefConfig(ctx, m)
	}
	base.NewRelationObject(log, base.DeleteRelationType, base.AppInstanceConfigLogType, appInstID.String(),
		base.VolumeRefConfigLogType, key).Noticef("App instance to volume relation.")
	log.Functionf("MaybeRemoveVolumeRefConfig done for %s", key)
}

func lookupVolumeRefConfig(ctx *zedmanagerContext, key string) *types.VolumeRefConfig {

	pub := ctx.pubVolumeRefConfig
	c, _ := pub.Get(key)
	if c == nil {
		log.Tracef("lookupVolumeRefConfig(%s) not found", key)
		return nil
	}
	config := c.(types.VolumeRefConfig)
	return &config
}

func getAllVolumeRefConfig(ctx *zedmanagerContext) []types.VolumeRefConfig {
	pub := ctx.pubVolumeRefConfig
	items := pub.GetAll()
	var configs []types.VolumeRefConfig
	for _, st := range items {
		config := st.(types.VolumeRefConfig)
		configs = append(configs, config)
	}
	return configs
}

func lookupVolumeRefStatus(ctx *zedmanagerContext, key string) *types.VolumeRefStatus {

	sub := ctx.subVolumeRefStatus
	c, _ := sub.Get(key)
	if c == nil {
		log.Tracef("lookupVolumeRefStatus(%s) not found", key)
		return nil
	}
	status := c.(types.VolumeRefStatus)
	return &status
}

func publishVolumeRefConfig(ctx *zedmanagerContext, config *types.VolumeRefConfig) {

	key := config.Key()
	log.Tracef("publishVolumeRefConfig(%s)", key)
	pub := ctx.pubVolumeRefConfig
	pub.Publish(key, *config)
	log.Tracef("publishVolumeRefConfig(%s) Done", key)
}

func unpublishVolumeRefConfig(ctx *zedmanagerContext, key string) {

	log.Tracef("unpublishVolumeRefConfig(%s)", key)
	pub := ctx.pubVolumeRefConfig
	c, _ := pub.Get(key)
	if c == nil {
		log.Errorf("unpublishVolumeRefConfig(%s) not found", key)
		return
	}
	pub.Unpublish(key)
	log.Tracef("unpublishVolumeRefConfig(%s) Done", key)
}

func handleVolumeRefStatusCreate(ctxArg interface{}, key string,
	statusArg interface{}) {
	handleVolumeRefStatusImpl(ctxArg, key, statusArg)
}

func handleVolumeRefStatusModify(ctxArg interface{}, key string,
	statusArg interface{}, oldStatusArg interface{}) {
	handleVolumeRefStatusImpl(ctxArg, key, statusArg)
}

func handleVolumeRefStatusImpl(ctxArg interface{}, key string,
	statusArg interface{}) {

	status := statusArg.(types.VolumeRefStatus)
	ctx := ctxArg.(*zedmanagerContext)
	log.Functionf("handleVolumeRefStatusImpl: key:%s, name:%s",
		key, status.DisplayName)
	pub := ctx.pubAppInstanceStatus
	items := pub.GetAll()
	for _, st := range items {
		aiStatus := st.(types.AppInstanceStatus)
		for _, vrs := range aiStatus.VolumeRefStatusList {
			if vrs.GenerationCounter == status.GenerationCounter &&
				vrs.LocalGenerationCounter == status.LocalGenerationCounter &&
				vrs.VolumeID == status.VolumeID {

				updateAIStatusUUID(ctx, aiStatus.UUIDandVersion.UUID.String())
			}
		}
	}
	log.Functionf("handleVolumeRefStatusImpl done for %s", key)
}

func handleVolumeRefStatusDelete(ctxArg interface{}, key string,
	statusArg interface{}) {

	status := statusArg.(types.VolumeRefStatus)
	ctx := ctxArg.(*zedmanagerContext)
	log.Functionf("handleVolumeRefStatusDelete: key:%s, name:%s",
		key, status.DisplayName)
	pub := ctx.pubAppInstanceStatus
	items := pub.GetAll()
	for _, st := range items {
		aiStatus := st.(types.AppInstanceStatus)
		for _, vrs := range aiStatus.VolumeRefStatusList {
			if vrs.GenerationCounter == status.GenerationCounter &&
				vrs.LocalGenerationCounter == status.LocalGenerationCounter &&
				vrs.VolumeID == status.VolumeID {

				updateAIStatusUUID(ctx, aiStatus.UUIDandVersion.UUID.String())
			}
		}
	}
	log.Functionf("handleVolumeRefStatusDelete done for %s", key)
}

func getVolumeRefStatusFromAIStatus(status *types.AppInstanceStatus,
	vrc types.VolumeRefConfig) *types.VolumeRefStatus {

	log.Tracef("getVolumeRefStatusFromAIStatus(%v)", vrc.Key())
	for i := range status.VolumeRefStatusList {
		vrs := &status.VolumeRefStatusList[i]
		if vrs.VolumeID == vrc.VolumeID &&
			vrs.GenerationCounter == vrc.GenerationCounter &&
			vrs.LocalGenerationCounter == vrc.LocalGenerationCounter {
			log.Tracef("getVolumeRefStatusFromAIStatus(%v) found %s "+
				"generationCounter %d localGenerationCounter %d",
				vrs.Key(), vrs.DisplayName, vrs.GenerationCounter,
				vrs.LocalGenerationCounter)
			return vrs
		}
	}
	log.Tracef("getVolumeRefStatusFromAIStatus(%v) Done", vrc.Key())
	return nil
}

func getVolumeRefConfigFromAIConfig(config *types.AppInstanceConfig,
	vrs types.VolumeRefStatus) *types.VolumeRefConfig {

	log.Tracef("getVolumeRefConfigFromAIConfig(%v)", vrs.Key())
	for i := range config.VolumeRefConfigList {
		vrc := &config.VolumeRefConfigList[i]
		if vrc.VolumeID == vrs.VolumeID &&
			vrc.GenerationCounter == vrs.GenerationCounter &&
			vrc.LocalGenerationCounter == vrs.LocalGenerationCounter {
			log.Tracef("getVolumeRefConfigFromAIConfig(%v) found %s "+
				"generationCounter %d localGenerationCounter %d",
				vrs.Key(), vrs.DisplayName, vrs.GenerationCounter,
				vrs.LocalGenerationCounter)
			return vrc
		}
	}
	log.Tracef("getVolumeRefConfigFromAIConfig(%v) Done", vrs.Key())
	return nil
}

/* Handlers for VolumesSnapshotStatus */

func handleVolumesSnapshotStatusCreate(ctx interface{}, key string, status interface{}) {
	log.Noticef("handleVolumesSnapshotStatusCreate")
	volumesSnapshotStatus := status.(types.VolumesSnapshotStatus)
	zedmanagerCtx := ctx.(*zedmanagerContext)
	appInstanceStatus := lookupAppInstanceStatus(zedmanagerCtx, volumesSnapshotStatus.AppUUID.String())
	if appInstanceStatus == nil {
		log.Errorf("handleVolumesSnapshotStatusCreate: AppInstanceStatus not found for %s", volumesSnapshotStatus.AppUUID.String())
		return
	}
	if volumesSnapshotStatus.HasError() {
		appInstanceStatus.Error = volumesSnapshotStatus.Error
		appInstanceStatus.ErrorTime = volumesSnapshotStatus.ErrorTime
		setSnapshotStatusError(appInstanceStatus, volumesSnapshotStatus.SnapshotID, volumesSnapshotStatus.ErrorDescription)
		publishAppInstanceStatus(zedmanagerCtx, appInstanceStatus)
		return
	}
	log.Noticef("Snapshot %s created", volumesSnapshotStatus.SnapshotID)
	err := moveSnapshotToAvailable(appInstanceStatus, volumesSnapshotStatus)
	if err != nil {
		errDesc := types.ErrorDescription{}
		errDesc.Error = err.Error()
		log.Errorf("handleVolumesSnapshotStatusCreate: %s", errDesc.Error)
		setSnapshotStatusError(appInstanceStatus, volumesSnapshotStatus.SnapshotID, errDesc)
		appInstanceStatus.SetErrorWithSourceAndDescription(errDesc, types.SnapshotInstanceStatus{})
	}
	publishAppInstanceStatus(zedmanagerCtx, appInstanceStatus)
}

func handleVolumesSnapshotStatusModify(ctx interface{}, key string, status interface{}, status2 interface{}) {
	log.Noticef("handleVolumesSnapshotStatusModify")
	// Reaction to a snapshot rollback
	volumesSnapshotStatus := status.(types.VolumesSnapshotStatus)
	zedmanagerCtx := ctx.(*zedmanagerContext)
	appInstanceStatus := lookupAppInstanceStatus(zedmanagerCtx, volumesSnapshotStatus.AppUUID.String())
	if appInstanceStatus == nil {
		log.Errorf("handleVolumesSnapshotStatusModify: AppInstanceStatus not found for %s", volumesSnapshotStatus.AppUUID.String())
		return
	}
	if volumesSnapshotStatus.HasError() {
		log.Errorf("Snapshot handling %s failed: %s", volumesSnapshotStatus.SnapshotID, volumesSnapshotStatus.Error)
		appInstanceStatus.SetErrorWithSourceAndDescription(volumesSnapshotStatus.ErrorDescription, volumesSnapshotStatus.ErrorSourceType)
		setSnapshotStatusError(appInstanceStatus, volumesSnapshotStatus.SnapshotID, volumesSnapshotStatus.ErrorDescription)
		publishAppInstanceStatus(zedmanagerCtx, appInstanceStatus)
		return
	}
	appInstanceStatus.SnapStatus.RollbackInProgress = false
	publishAppInstanceStatus(zedmanagerCtx, appInstanceStatus)
}

func handleVolumesSnapshotStatusDelete(ctx interface{}, key string, status interface{}) {
	log.Noticef("handleVolumesSnapshotStatusDelete")
	volumesSnapshotStatus := status.(types.VolumesSnapshotStatus)
	zedmanagerCtx := ctx.(*zedmanagerContext)
	appInstanceStatus := lookupAppInstanceStatus(zedmanagerCtx, volumesSnapshotStatus.AppUUID.String())
	if appInstanceStatus == nil {
		log.Errorf("handleVolumesSnapshotStatusDelete: AppInstanceStatus not found for %s", volumesSnapshotStatus.AppUUID.String())
		return
	}
	if volumesSnapshotStatus.HasError() {
		appInstanceStatus.Error = volumesSnapshotStatus.Error
		appInstanceStatus.ErrorTime = volumesSnapshotStatus.ErrorTime
		setSnapshotStatusError(appInstanceStatus, volumesSnapshotStatus.SnapshotID, volumesSnapshotStatus.ErrorDescription)
		publishAppInstanceStatus(zedmanagerCtx, appInstanceStatus)
		return
	}
	deleteSnapshotFromStatus(appInstanceStatus, volumesSnapshotStatus.SnapshotID)
	// Delete the serialized config, if it exists
	configDir := getSnapshotDir(volumesSnapshotStatus.SnapshotID)
	// delete the directory if it exists
	if _, err := os.Stat(configDir); err == nil {
		log.Noticef("Deleting snapshot directory %s", configDir)
		err = os.RemoveAll(configDir)
		if err != nil {
			log.Errorf("handleVolumesSnapshotStatusDelete: Failed to delete snapshot directory %s: %s", configDir, err)
			return
		}
	}

	log.Noticef("Deleting snapshot from the App status")
	publishAppInstanceStatus(zedmanagerCtx, appInstanceStatus)
}

/* Helper functions for the VolumesSnapshotStatus handlers */

func setSnapshotStatusError(aiStatus *types.AppInstanceStatus, snapshotID string, errDesc types.ErrorDescription) {
	snapshotStatus := lookupAvailableSnapshot(aiStatus, snapshotID)
	if snapshotStatus == nil {
		log.Errorf("setSnapshotStatusError: %s not found", snapshotID)
		return
	}
	if errDesc.ErrorTime.IsZero() {
		errDesc.ErrorTime = time.Now()
	}
	snapshotStatus.Error = errDesc
}

func lookupAvailableSnapshot(status *types.AppInstanceStatus, id string) *types.SnapshotInstanceStatus {
	log.Noticef("lookupAvailableSnapshot")
	for i, snap := range status.SnapStatus.AvailableSnapshots {
		if snap.Snapshot.SnapshotID == id {
			return &status.SnapStatus.AvailableSnapshots[i]
		}
	}
	return nil
}

func moveSnapshotToAvailable(status *types.AppInstanceStatus, volumesSnapshotStatus types.VolumesSnapshotStatus) error {
	log.Noticef("moveSnapshotToAvailable")
	// Remove from RequestedSnapshots
	snapToBeMoved := removeSnapshotFromSlice(&status.SnapStatus.RequestedSnapshots, volumesSnapshotStatus.SnapshotID)
	if snapToBeMoved == nil {
		log.Errorf("moveSnapshotToAvailable: Snapshot %s not found in RequestedSnapshots", volumesSnapshotStatus.SnapshotID)
		return fmt.Errorf("snapshot %s not found in RequestedSnapshots", volumesSnapshotStatus.SnapshotID)
	}
	// Update the time created from the volumesSnapshotStatus
	snapToBeMoved.TimeCreated = volumesSnapshotStatus.TimeCreated
	// Mark as reported
	snapToBeMoved.Reported = true
	// Add to AvailableSnapshots
	status.SnapStatus.AvailableSnapshots = append(status.SnapStatus.AvailableSnapshots, *snapToBeMoved)
	log.Noticef("Snapshot %s moved to AvailableSnapshots", volumesSnapshotStatus.SnapshotID)
	return nil
}

func removeSnapshotFromSlice(slice *[]types.SnapshotInstanceStatus, id string) (removedSnap *types.SnapshotInstanceStatus) {
	removedSnap = nil
	for i, snap := range *slice {
		if snap.Snapshot.SnapshotID == id {
			removedSnap = &snap
			*slice = append((*slice)[:i], (*slice)[i+1:]...)
			return removedSnap
		}
	}
	return nil
}

func deleteSnapshotFromStatus(status *types.AppInstanceStatus, id string) {
	log.Noticef("Deleting snapshot %s from status", id)
	// Remove the snapshot from the list of snapshots to be taken. This is needed in case the snapshot has not been
	// triggered yet. It's a valid case, as the snapshot might have been configured to be taken only during the app
	// upgrade. It's still ok if there is no such snapshot in the list, as it means that the snapshot has already been
	// triggered. That's why we do not check the return value of the function.
	_ = removeSnapshotFromSlice(&status.SnapStatus.RequestedSnapshots, id)
	// Remove the snapshot from the list of available snapshots. This is needed in case the snapshot has already been
	// triggered. It's still a valid case when there is no such snapshot in the list, as it means that the snapshot has
	// not been triggered yet. That's why we do not check the return value of the function.
	_ = removeSnapshotFromSlice(&status.SnapStatus.AvailableSnapshots, id)
	// Remove the snapshot from the list of snapshots to be triggered. This is needed in case the snapshot has already
	// been marked to be triggered, but the message has not been sent to volumemanager yet.
	removePreparedVolumesSnapshotConfig(status, id)
}

func restoreConfigFromSnapshot(ctx *zedmanagerContext, appInstanceStatus *types.AppInstanceStatus) (*types.AppInstanceConfig, error) {
	log.Noticef("restoreConfigFromSnapshot")

	// Get the snapshot status from the available snapshots
	snapshotID := appInstanceStatus.SnapStatus.ActiveSnapshot
	snapshotStatus := lookupAvailableSnapshot(appInstanceStatus, snapshotID)
	if snapshotStatus == nil {
		return nil, fmt.Errorf("SnapshotInstanceStatus not found for %s", snapshotID)
	}
	// Get the app instance config from the snapshot
	snappedAppInstanceConfig := deserializeConfigFromSnapshot(snapshotStatus)
	if snappedAppInstanceConfig == nil {
		return nil, fmt.Errorf("failed to read AppInstanceConfig from file for %s", snapshotID)
	}
	// Get the app instance config from the app instance status
	currentAppInstanceConfig := lookupAppInstanceConfig(ctx, appInstanceStatus.Key())
	if currentAppInstanceConfig == nil {
		return nil, fmt.Errorf("AppInstanceConfig not found for %s", appInstanceStatus.Key())
	}
	// Sync the information about available snapshots
	snappedAppInstanceConfig.Snapshot.Snapshots = make([]types.SnapshotDesc, len(currentAppInstanceConfig.Snapshot.Snapshots))
	copy(snappedAppInstanceConfig.Snapshot.Snapshots, currentAppInstanceConfig.Snapshot.Snapshots)
	// Restore the restart and purge commands counters
	snappedAppInstanceConfig.PurgeCmd = currentAppInstanceConfig.PurgeCmd
	snappedAppInstanceConfig.LocalPurgeCmd = currentAppInstanceConfig.LocalPurgeCmd
	snappedAppInstanceConfig.RestartCmd = currentAppInstanceConfig.RestartCmd
	snappedAppInstanceConfig.LocalRestartCmd = currentAppInstanceConfig.LocalRestartCmd
	return snappedAppInstanceConfig, nil
}

// deserializeConfigFromSnapshot deserializes the config from a file
func deserializeConfigFromSnapshot(status *types.SnapshotInstanceStatus) *types.AppInstanceConfig {
	log.Noticef("deserializeConfigFromSnapshot")
	dirname := getSnapshotDir(status.Snapshot.SnapshotID)
	filename := path.Join(dirname, types.SnapshotConfigFilename)
	var appInstanceConfig types.AppInstanceConfig
	configFile, err := os.Open(filename)
	if err != nil {
		log.Errorf("deserializeConfigFromSnapshot: Open failed %s", err)
		return nil
	}
	defer configFile.Close()
	jsonParser := json.NewDecoder(configFile)
	if err = jsonParser.Decode(&appInstanceConfig); err != nil {
		log.Errorf("deserializeConfigFromSnapshot: Decode failed %s", err)
		return nil
	}
	return &appInstanceConfig
}

/* Functions to publish/unpublish/lookup VolumesSnapshotConfig */

func publishVolumesSnapshotConfig(ctx *zedmanagerContext, t *types.VolumesSnapshotConfig) {
	key := t.Key()
	log.Tracef("publishVolumesSnapshotConfig(%s)", key)
	pub := ctx.pubVolumesSnapConfig
	_ = pub.Publish(key, *t)
}

func unpublishVolumesSnapshotConfig(ctx *zedmanagerContext, t *types.VolumesSnapshotConfig) {
	key := t.Key()
	log.Tracef("unpublishVolumesSnapshotConfig(%s)", key)
	pub := ctx.pubVolumesSnapConfig
	_ = pub.Unpublish(key)
}

func lookupVolumesSnapshotConfig(ctx *zedmanagerContext, snapshot string) *types.VolumesSnapshotConfig {
	key := snapshot
	pub := ctx.pubVolumesSnapConfig
	c, _ := pub.Get(key)
	if c == nil {
		log.Errorf("lookupVolumesSnapshotConfig(%s) not found", key)
		return nil
	}
	config := c.(types.VolumesSnapshotConfig)
	return &config
}
