// Copyright (c) 2017 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

// radvd configlet for overlay interface towards domU

package zedrouter

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/lf-edge/eve/pkg/pillar/agentlog"
)

// Need to fill in the overlay inteface name
const radvdTemplate = `
# Automatically generated by zedrouter
# Low preference to allow underlay to have high preference default
interface %s {
	IgnoreIfMissing on;
	AdvSendAdvert on;
	MaxRtrAdvInterval 1800;
	AdvManagedFlag on;
	AdvLinkMTU 1280;
	AdvDefaultPreference low;
	route fd00::/8
	{
		AdvRoutePreference high;
		AdvRouteLifetime 1800;
	};
};
`

// Create the radvd config file for the overlay
// Would be more polite to return an error then to Fatal
//	olIfname - Overlay Interface Name
func createRadvdConfiglet(cfgPathname string, olIfname string) {

	log.Tracef("createRadvdConfiglet: %s\n", olIfname)
	file, err := os.Create(cfgPathname)
	if err != nil {
		log.Fatal("createRadvdConfiglet failed ", err)
	}
	defer file.Close()
	file.WriteString(fmt.Sprintf(radvdTemplate, olIfname))
}

func deleteRadvdConfiglet(cfgPathname string) {

	log.Tracef("createRadvdConfiglet: %s\n", cfgPathname)
	if err := os.Remove(cfgPathname); err != nil {
		log.Errorln(err)
	}
}

// Run this:
//    radvd -u radvd -C /run/zedrouter/radvd.${OLIFNAME}.conf -p /run/radvd.${OLIFNAME}.pid
func startRadvd(cfgPathname string, olIfname string) {

	log.Tracef("startRadvd: %s\n", cfgPathname)
	pidPathname := "/run/radvd." + olIfname + ".pid"
	cmd := "nohup"
	args := []string{
		"radvd",
		"-u",
		"radvd",
		"-C",
		cfgPathname,
		"-p",
		pidPathname,
	}
	log.Functionf("Creating %s at %s", "nohup radvd", agentlog.GetMyStack())
	log.Functionf("Calling command %s %v\n", cmd, args)
	go exec.Command(cmd, args...).Output()
}

func getBridgeRadvdCfgFileName(bridgeName string) (string, string) {
	cfgFilename := "radvd." + bridgeName + ".conf"
	cfgPathname := runDirname + "/" + cfgFilename
	return cfgFilename, cfgPathname
}

//    pkill -u radvd -f radvd.${OLIFNAME}.conf
func stopRadvd(bridgeName string, printOnError bool) {
	cfgFilename, cfgPathname := getBridgeRadvdCfgFileName(bridgeName)

	log.Tracef("stopRadvd: cfgFileName:%s, cfgPathName:%s\n",
		cfgFilename, cfgPathname)
	pkillUserArgs("radvd", cfgFilename, printOnError)
	deleteRadvdConfiglet(cfgPathname)
}

func restartRadvdWithNewConfig(bridgeName string) {
	cfgFilename, cfgPathname := getBridgeRadvdCfgFileName(bridgeName)

	// kill existing radvd instance
	stopRadvd(cfgFilename, false)
	createRadvdConfiglet(cfgPathname, bridgeName)
	startRadvd(cfgPathname, bridgeName)
}
