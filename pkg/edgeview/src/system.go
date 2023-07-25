// Copyright (c) 2021 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"
	"golang.org/x/sys/unix"
)

const (
	tarMaxBytes = 512000000 // max tar directory of 512 Mbytes data
	tarTmpDir   = "/persist/tmp/"
)

var tarBlockDirs = []string{
	"/persist/clear",
	"/persist/vault",
	"/run/domainmgr/cloudinit",
	"/run/.kube/k3s",
}

var tarBlockFileSuffix = []string{
	".key.pem",
	".key",
}

func runSystem(cmds cmdOpt, sysOpt string) {
	opts, err := checkOpts(sysOpt, sysopts)
	if err != nil {
		fmt.Println("runSystem:", err)
	}

	for _, opt := range opts {
		printTitle("\n === System: <"+opt+"> ===\n\n", colorPURPLE, false)
		if opt == "newlog" {
			getLogStats()
		} else if opt == "volume" {
			getVolume()
		} else if opt == "app" {
			getSysApp()
		} else if opt == "datastore" {
			getDataStore()
		} else if opt == "cipher" {
			getCipher()
		} else if opt == "configitem" {
			runConfigItems()
		} else if opt == "download" {
			getDownload()
		} else if strings.HasPrefix(opt, "ps/") || opt == "ps" {
			runPS(opt)
		} else if strings.HasPrefix(opt, "cp/") {
			runCopy(opt)
		} else if strings.HasPrefix(opt, "cat/") {
			runCat(opt, cmds.Extraline)
		} else if strings.HasPrefix(opt, "du/") {
			runDu(opt)
		} else if strings.HasPrefix(opt, "ls/") {
			runLs(opt)
		} else if strings.HasPrefix(opt, "usb") {
			runUSB()
		} else if strings.HasPrefix(opt, "pci") {
			runPCI()
		} else if strings.HasPrefix(opt, "model") {
			getModel()
		} else if strings.HasPrefix(opt, "hw") {
			getHW()
		} else if strings.HasPrefix(opt, "top") {
			getTop(cmds.Extraline)
		} else if strings.HasPrefix(opt, "lastreboot") {
			getLastReboot()
		} else if strings.HasPrefix(opt, "techsupport") {
			runTechSupport(cmds, false)
		} else if strings.HasPrefix(opt, "dmesg") {
			getDmesg()
		} else if strings.HasPrefix(opt, "tar/") {
			getTarFile(opt)
		} else {
			fmt.Printf("opt %s: not supported yet\n", opt)
		}
		if isTechSupport {
			closePipe(true)
		}
	}
}

// getLogStats - in 'runSystem'
func getLogStats() {
	retData1, err := os.ReadFile("/run/newlogd/NewlogMetrics/global.json")
	if err == nil {
		prettyJSON, err := formatJSON(retData1)
		if err == nil {
			printColor(" - log stats:\n", colorCYAN)
			fmt.Printf("%s\n", prettyJSON)
		}
	}

	path := "/persist/newlog"
	info, err := os.Lstat(path)
	if err == nil {
		size := du(path, info)
		msg := fmt.Sprintf("\n newlog files total size: %d\n", size)
		printColor(msg, colorGREEN)
	}

	printColor(" log file directories:\n", colorCYAN)
	for _, d := range logdirectory {
		files, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		fmt.Printf(" %s: number of gzip files: %d\n", d, len(files))
		app := 0
		dev := 0
		var tmin, tmax, appmin, appmax int64
		for _, l := range files {
			var isApp, isDev bool
			if strings.HasPrefix(l.Name(), "app.") {
				app++
				isApp = true
			} else if strings.HasPrefix(l.Name(), "dev.") {
				dev++
				isDev = true
			}

			time1 := getFileTime(l.Name())
			if time1 == 0 {
				continue
			}
			if isDev && (tmin == 0 || tmin > time1) {
				tmin = time1
			}
			if isDev && (tmax == 0 || tmax < time1) {
				tmax = time1
			}
			if isApp && (appmin == 0 || appmin > time1) {
				appmin = time1
			}
			if isApp && (appmax == 0 || appmax < time1) {
				appmax = time1
			}
		}
		if app == 0 && dev == 0 {
			fmt.Printf("  directory empty\n")
		} else {
			fmt.Printf("  dev files: %d, app files: %d \n", dev, app)
			if tmin > 0 || tmax > 0 {
				fmt.Printf("   dev-earliest: %v, dev-latest: %v\n", time.Unix(tmin, 0).Format(time.RFC3339), time.Unix(tmax, 0).Format(time.RFC3339))
			}
			if appmin > 0 || appmax > 0 {
				fmt.Printf("   app-earlist: %v, app-latest: %v\n", time.Unix(appmin, 0).Format(time.RFC3339), time.Unix(appmax, 0).Format(time.RFC3339))
			}
		}
	}
	fmt.Println()
}

func getDmesg() {
	printTitle("Dmesg:", colorCYAN, false)
	buf := make([]byte, 64*1024)
	_, _, err := unix.Syscall(unix.SYS_SYSLOG, uintptr(3), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if err == 0 {
		size := 0
		lines := bytes.SplitAfter(buf, []byte("\n"))
		for _, l := range lines {
			printDmesgLine(string(l))
			size += len(l)
			if size > 4096 {
				size = 0
				closePipe(true)
			}
		}
	}
}

func printDmesgLine(line string) {
	severity := 6 // info
	sline := strings.SplitN(line[1:], ">", 2)
	var bColon string
	if len(sline) == 2 {
		var aColon string
		sev, err := strconv.Atoi(sline[0])
		if err == nil {
			severity = sev
		}
		sect := strings.SplitAfterN(sline[1], "]", 3)
		if len(sect) == 3 {
			Colons := strings.SplitAfterN(sect[2], ":", 2)
			if len(Colons) != 2 {
				aColon = sect[2]
			} else {
				bColon = Colons[0]
				aColon = Colons[1]
			}
			fmt.Printf(colorCYAN, sect[0])
			fmt.Printf(colorYELLOW, bColon)
		} else {
			aColon = sline[1]
		}
		if severity <= 3 { // Err and above
			fmt.Printf(colorRED, aColon)
		} else if severity <= 4 { // Warn
			fmt.Printf(colorPURPLE, aColon)
		} else {
			fmt.Printf("%s", aColon)
		}
	} else {
		fmt.Printf("%s", line)
	}
}

func runDu(opt string) {
	dirs := strings.SplitN(opt, "du/", 2)
	if len(dirs) != 2 {
		fmt.Printf("du/<directory name> is not valid\n")
		return
	}
	dir := dirs[1]
	finfo, err := os.Stat(dir)
	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}

	if !finfo.IsDir() {
		fmt.Printf("%s is not a directory\n", opt)
		return
	}

	printColor(" - Disk Usage: "+dir, colorCYAN)
	sizeB := du(dir, finfo)
	sizeK := float64(sizeB) / 1024
	sizeM := sizeK / 1024
	sizeG := sizeM / 1024
	duSize := fmt.Sprintf("%d Bytes", sizeB)
	if sizeG > 1.0 {
		duSize = fmt.Sprintf("%.2f (GBytes)", sizeG)
	} else if sizeM > 1.0 {
		duSize = fmt.Sprintf("%.2f (MBytes)", sizeM)
	} else if sizeK > 1.0 {
		duSize = fmt.Sprintf("%.2f (KBytes)", sizeK)
	}
	fmt.Printf(" %s\n", duSize)
}

func du(currentPath string, info os.FileInfo) int64 {
	size := info.Size()
	if !info.IsDir() {
		return size
	}

	dir, err := os.Open(currentPath)
	if err != nil {
		fmt.Println(err)
		return size
	}
	defer dir.Close()

	fis, err := dir.Readdir(-1)
	if err != nil {
		fmt.Println(err)
		return 0
	}
	for _, fi := range fis {
		if fi.Name() == "." || fi.Name() == ".." {
			continue
		}
		size += du(currentPath+"/"+fi.Name(), fi)
	}

	return size
}

func getFileTime(filename string) int64 {
	var fn []string
	if strings.Contains(filename, ".gz") && strings.Contains(filename, ".log.") {
		fn = strings.Split(filename, ".gz")
	}
	if len(fn) < 2 {
		return 0
	}
	fn = strings.Split(fn[0], ".log.")
	if len(fn) < 2 {
		return 0
	}
	filetime, _ := strconv.Atoi(fn[1])
	return int64(filetime / 1000)
}

func getVolume() {
	jfiles, err := listJSONFiles("/run/zedagent/AppInstanceConfig")
	if err != nil {
		return
	}

	for _, line := range jfiles {
		retbytes, err := os.ReadFile(line)
		if err != nil {
			continue
		}
		var appinst types.AppInstanceConfig
		_ = json.Unmarshal(retbytes, &appinst)
		for _, vol := range appinst.VolumeRefConfigList {
			printColor("\n - App "+appinst.DisplayName, colorCYAN)
			fmt.Printf("  volume config, ID: %s\n", vol.VolumeID.String())

			jfiles, err := listJSONFiles("/run/zedagent/VolumeConfig")
			if err != nil {
				continue
			}
			var foundfile string
			for _, file := range jfiles {
				if strings.HasSuffix(file, ".json") && strings.Contains(file, vol.VolumeID.String()) {
					foundfile = file
					break
				}
			}
			if foundfile == "" {
				continue
			}
			retbytes, err := os.ReadFile(foundfile)
			if err != nil {
				continue
			}
			var vol1 types.VolumeConfig
			_ = json.Unmarshal(retbytes, &vol1)
			fmt.Printf("   name: %s, ID %s, RefCount: %d \n", vol1.DisplayName, vol1.VolumeID.String(), vol1.RefCount)

			printColor("\n content tree config: "+vol1.ContentID.String(), colorBLUE)
			retbytes, err = os.ReadFile("/run/zedagent/ContentTreeConfig/" + vol1.ContentID.String() + ".json")
			var cont types.ContentTreeConfig
			_ = json.Unmarshal(retbytes, &cont)
			fmt.Printf("   url: %s, format: %s, sha: %s\n", cont.RelativeURL, cont.Format, cont.ContentSha256)
			fmt.Printf("   size: %d, name: %s\n", cont.MaxDownloadSize, cont.DisplayName)
		}
	}
}

func getSysApp() {

	getDevMemStats()

	jfiles, err := listJSONFiles("/run/zedrouter/AppNetworkStatus")
	if err != nil {
		return
	}
	for _, s := range jfiles {
		retbytes, _ := os.ReadFile(s)
		status := strings.TrimSuffix(string(retbytes), "\n")
		appuuid := doAppNet(status, "", true)
		retbytes, err := os.ReadFile("/run/domainmgr/DomainMetric/" + appuuid + ".json")
		if err == nil {
			var metric types.DomainMetric
			_ = json.Unmarshal(retbytes, &metric)
			fmt.Printf("    CPU: %d, Used Mem(MB): %d, Avail Mem(BM): %d\n",
				metric.CPUTotalNs, metric.UsedMemory, metric.AvailableMemory)
		}

		retbytes, err = os.ReadFile("/run/zedmanager/DomainConfig/" + appuuid + ".json")
		if err != nil {
			continue
		}
		printColor("\n  - vnc/log info:", colorGREEN)
		var config types.DomainConfig
		_ = json.Unmarshal(retbytes, &config)
		fmt.Printf("    VNC enabled: %v, VNC display id: %d, Applog disabled: %v\n",
			config.EnableVnc, config.VncDisplay, config.DisableLogs)
	}
}

func getDataStore() {
	jfiles, err := listJSONFiles("/run/zedagent/DatastoreConfig")
	if err != nil {
		return
	}

	printColor(" - DataStore:", colorCYAN)
	for _, l := range jfiles {
		retbytes1, err := os.ReadFile(l)
		if err != nil {
			continue
		}
		var data types.DatastoreConfig
		_ = json.Unmarshal(retbytes1, &data)
		if data.IsCipher {
			fmt.Printf("   Cipher Context ID: %s, Cipher Hash: %s\n",
				data.CipherContextID, base64.StdEncoding.EncodeToString(data.ClearTextHash))
		}
		fmt.Printf("\n   FQDN: %s, Path: %s, DS Type: %s, Is Cipher: %v\n", data.Fqdn, data.Dpath, data.DsType, data.IsCipher)
		if len(data.DsCertPEM) > 0 {
			for _, c := range data.DsCertPEM {
				printCert(c)
			}
		}
	}
}

func getModel() {
	printTitle("Model:", colorCYAN, false)
	prog := "spec.sh"
	var args []string
	_, _ = runCmd(prog, args, true)
}

func getHW() {
	printTitle("HW:", colorCYAN, false)
	err := addPackage("/usr/sbin/lshw", "lshw")
	if err != nil {
		fmt.Printf("add package: %v\n", err)
		return
	}
	prog := "lshw"
	args := []string{"-json"}
	_, _ = runCmd(prog, args, true)
}

func getLastReboot() {
	files, err := os.ReadDir("/persist/log")
	if err != nil {
		fmt.Printf("failed to get to /persist/log\n")
		return
	}

	now := time.Now().Unix()
	for _, l := range files {
		info, err := l.Info()
		if err != nil {
			fmt.Printf("failed to get file '%s' info: %v\n", l.Name(), err)
			continue
		}
		if now-info.ModTime().Unix() > 2592000 { // if files are older then 30 days
			continue
		}
		var rebootFile string
		if strings.Contains(l.Name(), "reboot-reason.log") {
			rebootFile = "reboot-reason.log"
		} else if strings.Contains(l.Name(), "reboot-stack.log") {
			rebootFile = "reboot-stack.log"
		} else {
			continue
		}
		printTitle(rebootFile, colorBLUE, false)
		if strings.Contains(rebootFile, "reason") {
			readAFile("/persist/log/"+rebootFile, -5)
		} else {
			readAFile("/persist/log/"+rebootFile, 0)
		}
	}

	files, err = os.ReadDir("/persist/newlog/panicStacks")
	if err != nil {
		fmt.Printf("failed to get to /persist/newlog/panicStacks\n")
		return
	}

	for _, l := range files {
		info, err := l.Info()
		if err != nil {
			fmt.Printf("failed to get file '%s' info: %v\n", l.Name(), err)
			continue
		}
		if now-info.ModTime().Unix() > 2592000 { // if files are older then 30 days
			continue
		}
		if strings.Contains(l.Name(), "pillar-panic-stack.") {
			fields := strings.Fields(l.Name())
			n := len(fields)
			if n < 1 {
				continue
			}
			retbytes, err := os.ReadFile("/persist/newlog/panicStacks/" + fields[n-1])
			if err != nil {
				break
			}
			printTitle("newlog pillar panicStack", colorBLUE, false)
			fmt.Printf("\n%s\n", string(retbytes))
			break
		}
	}
}

func runUSB() {
	printTitle("USB:", colorCYAN, false)
	prog := "apk"
	args := []string{"add", "usbutils"}
	_, _ = runCmd(prog, args, false)
	prog = "lsusb"
	args = []string{"-v"}
	retStr, err := runCmd(prog, args, false)
	if err != nil {
		fmt.Printf("%v\n", err)
	} else {
		fmt.Printf("%s\n", retStr)
	}
}

func runPCI() {
	printTitle("PCI:", colorCYAN, false)
	err := addPackage("/usr/sbin/lspci", "pciutils")
	if err != nil {
		fmt.Printf("add package: %v\n", err)
		return
	}
	prog := "lspci"
	args := []string{"-v"}
	retStr, err := runCmd(prog, args, false)
	if err != nil {
		fmt.Printf("%v\n", err)
	} else {
		fmt.Printf("%s\n", retStr)
	}
}

func getCipher() {
	path := "/persist/certs"
	files, err := os.ReadDir(path)
	if err == nil {
		printColor(" - /persist/certs:\n", colorCYAN)
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				fmt.Printf("file: %s, size %d\n", path+f.Name(), info.Size())
			}
		}
	}

	certType := map[types.CertType]string{
		types.CertTypeOnboarding:      "onboarding",
		types.CertTypeRestrictSigning: "signing",
		types.CertTypeEk:              "Ek",
		types.CertTypeEcdhXchange:     "EdchXchange",
	}

	printColor(" - Additional CA-Certificates:\n", colorCYAN)
	files, err = os.ReadDir("/etc/ssl/certs")
	if err == nil {
		for _, f := range files {
			if !strings.Contains(f.Name(), "/usr/local/share") {
				continue
			}
			fmt.Printf("%s\n", f)
		}
	}

	jfiles, err := listJSONFiles("/run/zedagent/DatastoreConfig/")
	if err == nil {
		printColor("\n - DataStore Config:", colorCYAN)
		for _, l := range jfiles {
			retbytes1, err := os.ReadFile(l)
			if err != nil {
				continue
			}
			var data types.DatastoreConfig
			_ = json.Unmarshal(retbytes1, &data)
			fmt.Printf(" %s:\n", getJSONFileID(l))
			fmt.Printf("  type: %s, FQDN: %s, ApiKey: %s, path: %s, Is Cipher: %v\n",
				data.DsType, data.Fqdn, data.ApiKey, data.Dpath, data.IsCipher)
			if len(data.DsCertPEM) > 0 {
				for _, c := range data.DsCertPEM {
					printCert(c)
				}
			}
		}
	}

	jfiles, err = listJSONFiles("/run/domainmgr/CipherBlockStatus")
	if err == nil {
		printColor("\n - Domainmgr CipherBlock:", colorCYAN)
		for _, l := range jfiles {
			retbytes1, err := os.ReadFile(l)
			if err != nil {
				continue
			}
			var data types.CipherBlockStatus
			_ = json.Unmarshal(retbytes1, &data)
			fmt.Printf(" %s:\n", getJSONFileID(l))
			fmt.Printf("  ID: %s, Is Cipher: %v\n", data.CipherBlockID, data.IsCipher)
			filename := "/run/domainmgr/cloudinit/" + data.CipherBlockID + ".cidata"
			_, err = os.Stat(filename)
			if err == nil {
				fmt.Printf("   cloudinit file: %s\n", filename)
			}
		}
	}

	jfiles, err = listJSONFiles("/persist/status/tpmmgr/EdgeNodeCert")
	if err == nil {
		printColor("\n - TPMmgr Edgenode Certs:", colorCYAN)
		for _, l := range jfiles {
			retbytes1, err := os.ReadFile(l)
			if err != nil {
				continue
			}
			var data types.EdgeNodeCert
			_ = json.Unmarshal(retbytes1, &data)
			fmt.Printf(" %s:\n", getJSONFileID(l))
			fmt.Printf("  hash Algo: %d, Cert ID: %s, Cert Type: %s, Is TPM: %v\n",
				data.HashAlgo, base64.StdEncoding.EncodeToString(data.CertID), certType[data.CertType], data.IsTpm)
			printCert(data.Cert)
		}
	}

	jfiles, err = listJSONFiles("/persist/status/zedagent/CipherContext")
	if err == nil {
		printColor("\n - Cipher Context:", colorCYAN)
		for _, l := range jfiles {
			retbytes1, err := os.ReadFile(l)
			if err != nil {
				continue
			}
			var data types.CipherContext
			_ = json.Unmarshal(retbytes1, &data)
			fmt.Printf(" %s:\n", getJSONFileID(l))
			fmt.Printf("  ID: %s, Device Cert Hash: %s\n",
				data.ContextID, base64.StdEncoding.EncodeToString(data.DeviceCertHash))
			fmt.Printf("  Controller Cert Hash: %s\n", base64.StdEncoding.EncodeToString(data.ControllerCertHash))
		}
	}

	jfiles, err = listJSONFiles("/persist/status/zedagent/ControllerCert")
	if err == nil {
		printColor("\n - Controller Certs:", colorCYAN)
		for _, l := range jfiles {
			retbytes1, err := os.ReadFile(l)
			if err != nil {
				continue
			}
			var data types.ControllerCert
			_ = json.Unmarshal(retbytes1, &data)
			fmt.Printf(" %s:\n", getJSONFileID(l))
			fmt.Printf("  hash Algo: %d, Type: %d, hash %s\n",
				data.HashAlgo, data.Type, base64.StdEncoding.EncodeToString(data.CertHash))
			printCert(data.Cert)
		}
	}
}

func printCert(certdata []byte) {
	block, _ := pem.Decode(certdata)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		fmt.Printf("cert err %v\n", err)
		return
	}
	fmt.Printf("    subject: %s, serial: %d, valid until: %v\n", cert.Subject, cert.SerialNumber, cert.NotAfter)
	fmt.Printf("     issuer: %s\n", cert.Issuer)
}

func runConfigItems() {
	printColor(" - global settings:", colorCYAN)

	// /hostfs is need here to read the memory limit for eve item
	configitems := getConfigItems()

	configMap := types.NewConfigItemSpecMap()
	// global
	for k, g := range configitems.GlobalSettings {
		//fmt.Printf("key: %s; value %v\n", k, g)
		s := configMap.GlobalSettings[k]
		s1 := types.ConfigItemValue{
			ItemType:      s.ItemType,
			IntValue:      s.IntDefault,
			StrValue:      s.StringDefault,
			BoolValue:     s.BoolDefault,
			TriStateValue: s.TriStateDefault,
		}
		if getCfgValue(g) == getCfgValue(s1) {
			buff := fmt.Sprintf(" %s: %s\n", k, getCfgValue(g))
			fmt.Printf("%s", buff)
		} else {
			buff := fmt.Sprintf(" %s: %s; default %s\n", k, getCfgValue(g), getCfgValue(s1))
			printColor(buff, colorYELLOW)
		}
		//fmt.Printf("   default: %v\n", configMap.GlobalSettings[k])
	}

	// agent
	printColor("\n - agent settings:", colorCYAN)
	for k, g := range configitems.AgentSettings {
		printColor("  "+k+":  ", colorRED)
		for k1, g1 := range g {
			fmt.Printf("    %s, %s\n", k1, getCfgValue(g1))
		}
	}
}

func getConfigItems() types.ConfigItemValueMap {
	var cfgItem types.ConfigItemValueMap
	retbytes, err := os.ReadFile("/persist/status/zedagent/ConfigItemValueMap/global.json")
	if err != nil {
		return cfgItem
	}
	_ = json.Unmarshal(retbytes, &cfgItem)
	return cfgItem
}

func getCfgValue(g types.ConfigItemValue) string {
	value := ""
	switch g.ItemType {
	case types.ConfigItemTypeInt:
		value = strconv.Itoa(int(g.IntValue))
	case types.ConfigItemTypeBool:
		value = strconv.FormatBool(g.BoolValue)
	case types.ConfigItemTypeString:
		value = g.StrValue
	case types.ConfigItemTypeTriState:
		if g.TriStateValue == types.TS_NONE {
			value = "None"
		} else if g.TriStateValue == types.TS_DISABLED {
			value = "Disabled"
		} else if g.TriStateValue == types.TS_ENABLED {
			value = "Enabled"
		} else {
			value = "un-supported"
		}
	default:
		value = "un-supported"
	}
	return value
}

func getDownload() {
	pubsubSvs("/run/", "volumemgr", "DownloaderConfig")
	pubsubSvs("/run/", "downloader", "DownloaderStatus")

	getMetricsMap("/run/downloader/MetricsMap/", nil, true)
	checkDownload("/persist/downloads")
	checkDownload("/persist/vault/downloader")
	checkDownload("/persist/vault/verifier")
}

func getTop(lines int) {
	prog := "top"
	args := []string{"-n", "1", "-b"}
	retStr, _ := runCmd(prog, args, false)
	if lines == 0 {
		fmt.Printf("%s\n", retStr)
	} else {
		lis := strings.SplitAfter(retStr, "\n")
		for i, l := range lis {
			if i < lines {
				fmt.Printf("%s", l)
			} else {
				break
			}
		}
	}
}

func runPS(opt string) {
	var item string

	if !rePattern.MatchString(opt) {
		return
	}
	if strings.Contains(opt, "ps/") {
		opts := strings.SplitN(opt, "ps/", 2)
		if len(opts) != 2 {
			return
		}
		item = opts[1]
	}

	processes, err := process.Processes()
	if err != nil {
		fmt.Printf("can not get processes\n")
		return
	}

	printColor(" - ps: PID Times VMS RSS CPU% MEM% Name Cmdline", colorCYAN)

	for _, p := range processes {
		cmd, err := p.Cmdline()
		if err != nil {
			fmt.Printf("%v\n", err)
			continue
		}
		if item != "" && !strings.Contains(cmd, item) {
			continue
		}
		t, err := p.Times()
		if err != nil {
			fmt.Printf("%v\n", err)
			continue
		}
		c, err := p.CPUPercent()
		if err != nil {
			fmt.Printf("%v\n", err)
			continue
		}
		m, err := p.MemoryPercent()
		if err != nil {
			fmt.Printf("%v\n", err)
			continue
		}
		mi, err := p.MemoryInfo()
		if err != nil {
			fmt.Printf("%v\n", err)
			continue
		}
		fmt.Printf("%06d: %v, %d, %d, %.3f, %.3f, %s\n", p.Pid, t, mi.VMS, mi.RSS, c, m, cmd)
	}
}

func checkDownload(dir string) {
	fis, err := findAllFileInfo(dir)
	if err != nil || len(fis) == 0 {
		return
	}
	for _, fi := range fis {
		fmt.Printf("file: %s, size: %d\n", fi.Name(), fi.Size())
	}
}

func runCat(opt string, line int) {
	execCmd := strings.SplitN(opt, "cat/", 2)
	if len(execCmd) != 2 {
		fmt.Printf("cat needs a / and command input\n")
		return
	}
	path := execCmd[1]
	printColor(" - cat cmd: "+path, colorCYAN)
	fi, err := os.Stat(path)
	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}

	ok := checkBlockedDirs(path)
	if !ok {
		fmt.Printf("directory is blocked for file display: %s\n", path)
		return
	}

	ok = checkBlockedFileSuffix(path)
	if !ok {
		fmt.Printf("file %s can not be displayed\n", path)
		return
	}

	if fi.IsDir() {
		fmt.Printf("can not cat a directory\n")
		return
	}
	readAFile(path, line)
}

func runLs(opt string) {
	execCmd := strings.SplitN(opt, "ls/", 2)
	if len(execCmd) != 2 {
		fmt.Printf("ls needs a / and command input\n")
		return
	}
	path := execCmd[1]
	printColor(" - ls cmd: "+path, colorCYAN)
	fi, err := os.Stat(path)
	var matchStr []string
	var hasPref, hasSuff string
	if err != nil {
		if strings.Contains(path, "*") {
			dir, file := filepath.Split(path)
			if !strings.Contains(file, "*") {
				fmt.Printf("dir has *. %v\n", err)
				return
			}
			fi, err = os.Stat(dir)
			if err != nil {
				fmt.Printf("os.Stat dir failed. %v\n", err)
				return
			}
			matchStr = strings.Split(file, "*")
			n := len(matchStr)
			if n < 2 {
				fmt.Printf("len %d, %v\n", len(matchStr), err)
				return
			}
			hasPref = matchStr[0]
			hasSuff = matchStr[n-1]
			path = dir
		} else {
			fmt.Printf("os.Stat failed. %v\n", err)
			return
		}
	}

	if fi.IsDir() {
		files, err := os.ReadDir(path)
		if err != nil {
			fmt.Printf("read dir failed. %v\n", err)
			return
		}
		for _, file := range files {
			if len(matchStr) > 0 {
				if hasPref != "" && !strings.HasPrefix(file.Name(), hasPref) ||
					(hasSuff != "" && !strings.HasSuffix(file.Name(), hasSuff)) {
					continue
				}
				var notmatch bool
				for _, m := range matchStr {
					if !strings.Contains(file.Name(), m) {
						notmatch = true
						break
					}
				}
				if notmatch {
					continue
				}
			}
			if info, err := file.Info(); err == nil {
				dispAFile(info)
			}
		}
	} else {
		dispAFile(fi)
	}
}

func readAFile(path string, extraline int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}

	buf, err := io.ReadAll(f)
	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}
	contentType := http.DetectContentType(buf)

	fmt.Printf("content type: %s\n", contentType)
	if extraline != 0 {
		lines := bytes.SplitAfter(buf, []byte("\n"))
		N := len(lines)
		buf = []byte{}
		var newlines [][]byte
		if extraline > 0 { // head
			end := N
			if extraline < N {
				end = extraline
			}
			newlines = lines[:end]
		} else { // tail
			start := 0
			if -(extraline) < N {
				start = N + extraline
			}
			newlines = lines[start:]
		}
		for _, l := range newlines {
			buf = append(buf, l...)
		}
	}
	maySplitAndPrint(buf)
	fmt.Printf("\n")
}

func dispAFile(f os.FileInfo) {
	fmt.Printf("%s, %v, %d, %s\n", f.Mode().String(), f.ModTime(), f.Size(), f.Name())
}

func getTarFile(opt string) {
	execCmd := strings.SplitN(opt, "tar/", 2)
	if len(execCmd) != 2 {
		fmt.Printf("tar needs a / and path input\n")
		return
	}
	source := filepath.Clean(execCmd[1])
	printColor(" - tar cmd: "+source, colorCYAN)
	closePipe(true)

	fi, err := os.Stat(source)
	if err != nil {
		fmt.Printf("find directory error: %v\n", err)
		return
	}

	if !fi.IsDir() {
		fmt.Printf("the path %s is not a directory\n", source)
		return
	}

	ok := checkBlockedDirs(source)
	if !ok {
		fmt.Printf("the directory %s can not be tarred\n", source)
		return
	}

	// check the directory size not to exceed the max
	sizeBytes := du(source, fi)
	if sizeBytes > tarMaxBytes {
		fmt.Printf("directory size %d, exceeds maximum %d\n", sizeBytes, tarMaxBytes)
		return
	}
	dirs := strings.Split(source, "/")
	if _, err := os.Stat(tarTmpDir); err != nil && os.IsNotExist(err) {
		if err := os.MkdirAll(tarTmpDir, 0755); err != nil {
			fmt.Printf("can not create %s\n", tarTmpDir)
			return
		}
	}
	targzfileName := tarTmpDir
	for _, d := range dirs {
		if d == "" {
			continue
		}
		targzfileName = targzfileName + d + "."
	}
	targzfileName = targzfileName + strconv.FormatInt(time.Now().Unix(), 10) + ".tar"
	archiveName := filepath.Base(targzfileName)
	targzfileName = targzfileName + ".gz"

	targzFile, err := os.Create(targzfileName)
	if err != nil {
		log.Errorf("can not create tar file")
		return
	}

	err = createArchive(source, archiveName, targzFile)
	targzFile.Close()
	if err != nil {
		_ = os.Remove(targzfileName)
		log.Errorf("tar oper error: %v", err)
		return
	}

	// copy the <name>.tar.gz file back to the user
	runCopy("cp/" + targzfileName)
	_ = os.Remove(targzfileName)
}

func createArchive(source, archiveName string, buf io.Writer) error {
	gzipWriter := gzip.NewWriter(buf)
	gzipWriter.Name = archiveName
	defer gzipWriter.Close()

	tarball := tar.NewWriter(gzipWriter)
	defer tarball.Close()

	baseDir := filepath.Base(source)
	err := filepath.Walk(source,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// skip file with suffix of '.key.pem' or '.key'
			ok := checkBlockedFileSuffix(info.Name())
			if !ok {
				return nil
			}

			// some files like socket type, can not be tarred
			if info.Size() == 0 && !info.Mode().IsRegular() {
				return nil
			}
			header, err := tar.FileInfoHeader(info, info.Name())
			if err != nil {
				return err
			}

			header.Name = filepath.Join(baseDir, strings.TrimPrefix(path, source))
			if err := tarball.WriteHeader(header); err != nil {
				return err
			}

			if info.IsDir() {
				// skip if it is part of the blocked directories
				if !checkBlockedDirs(filepath.Join(path, info.Name())) {
					return filepath.SkipDir
				}
				return nil
			}

			// skip symlink files
			if info.Mode()&os.ModeSymlink == os.ModeSymlink {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			_, err = io.Copy(tarball, file)
			return err
		})
	return err
}

// ungzipFile - runs on the client side
func ungzipFile(fname string) {
	source := fileCopyDir + fname
	reader, err := os.Open(source)
	if err != nil {
		fmt.Printf("ungzip open error %v\n", err)
		return
	}
	defer reader.Close()

	archive, err := gzip.NewReader(reader)
	if err != nil {
		fmt.Printf("ungzip error %v\n", err)
		return
	}
	defer archive.Close()

	target := filepath.Join(fileCopyDir, archive.Name)
	writer, err := os.Create(target)
	if err != nil {
		fmt.Printf("ungzip create file error %v\n", err)
		return
	}
	defer writer.Close()

	_, err = io.Copy(writer, archive)
	if err != nil {
		fmt.Printf("ungzip copy error %v\n", err)
		return
	}
	ifile, err := os.Stat(target)
	if err == nil {
		fmt.Printf("tar file (size %d) generated: %s\n", ifile.Size(), target)
	}
	_ = os.Remove(source)
}

// checkBlockedDirs - return false if the dirName is in blocked list
func checkBlockedDirs(dirName string) bool {
	for _, d := range tarBlockDirs {
		d1 := strings.TrimPrefix(d, "/") // handle without leading '/' in front
		if strings.HasPrefix(dirName, d) || strings.HasPrefix(dirName, d1) {
			return false
		}
	}
	return true
}

// checkBlockedFileSuffix - return false if the fileName suffix in blocked list
func checkBlockedFileSuffix(fileName string) bool {
	for _, f := range tarBlockFileSuffix {
		if strings.HasSuffix(fileName, f) {
			return false
		}
	}
	return true
}

func runTechSupport(cmds cmdOpt, isLocal bool) {
	var err error
	filedir := "/tmp/"
	if isLocal {
		filedir = "/run/edgeview/"
	}
	tsfileName := filedir + "techsupport-tmp-" + getFileTimeStr(time.Now())
	techSuppFile, err = os.Create(tsfileName)
	if err != nil {
		log.Errorf("can not create techsupport file")
		return
	}
	defer techSuppFile.Close()

	if isLocal {
		socketOpen = true // for closePipe() to work
	}
	closePipe(true)
	isTechSupport = true

	printTitle("\n       - Show Tech-Support -\n\n\n", colorYELLOW, false)

	getBasics()

	printTitle("\n       - network info -\n\n", colorRED, false)
	runNetwork("route,arp,if,acl,connectivity,url,socket,app,mdns,nslookup/google.com,trace/8.8.8.8,wireless,flow,showcerts")
	closePipe(true)

	printTitle("\n       - system info -\n\n", colorRED, false)
	runSystem(cmds, "hw,model,pci,usb,lastreboot,newlog,volume,app,datastore,cipher,dmesg,configitem")
	closePipe(true)

	printTitle("\n       - pub/sub info -\n\n", colorRED, false)
	runPubsub("nim,domainmgr,nodeagent,baseosmgr,tpmmgr,global,vaultmgr,volumemgr,zedagent,zedmanager,zedrouter,zedclient,zfsmanager,edgeview,watcher")

	printTitle("\n       - Done Tech-Support -\n\n", colorYELLOW, false)
	closePipe(true)

	isTechSupport = false
	techSuppFile.Close()

	gzipfileName, err := gzipTechSuppFile(tsfileName)
	if err == nil {
		if isLocal {
			_ = os.Remove(tsfileName)
			_ = os.Remove(types.EdgeviewPath + "run-techsupport")
			return
		}
		runCopy("cp/" + gzipfileName)
	} else {
		log.Errorf("gzip techsupport file failed, %v", err)
		if isLocal {
			_ = os.Remove(types.EdgeviewPath + "run-techsupport")
		}
	}

	_ = os.Remove(tsfileName)
	if gzipfileName != "" {
		_ = os.Remove(gzipfileName)
	}
}

func gzipTechSuppFile(ifileName string) (string, error) {
	var ofileName string
	ifile, err := os.Open(ifileName)
	if err != nil {
		log.Errorf("can not open file %v", err)
		return ofileName, err
	}

	reader := bufio.NewReader(ifile)
	content, _ := io.ReadAll(reader)

	tmpfiles := strings.Split(ifileName, "-tmp-")
	if len(tmpfiles) != 2 {
		return ofileName, fmt.Errorf("filename format incorrect")
	}

	ofile, err := os.Create(tmpfiles[0] + "-" + tmpfiles[1] + ".gz")
	if err != nil {
		log.Errorf("can not create file %v", err)
		return ofileName, err
	}

	ofileName = ofile.Name()
	gw, _ := gzip.NewWriterLevel(ofile, gzip.BestCompression)
	_, err = gw.Write(content)
	if err != nil {
		log.Errorf("gzip write error: %v", err)
		return ofileName, err
	}
	err = gw.Close()
	if err != nil {
		log.Errorf("gzip close error: %v", err)
		return ofileName, err
	}

	err = ofile.Sync()
	if err != nil {
		log.Errorf("file sync error: %v", err)
		return ofileName, err
	}

	err = ofile.Close()
	if err != nil {
		log.Errorf("file close error: %v", err)
		return ofileName, err
	}
	return ofileName, nil
}

func getDevInfo() types.EdgeNodeInfo {
	var devInfo types.EdgeNodeInfo
	jfiles, err := listJSONFiles("/persist/status/zedagent/EdgeNodeInfo")
	if err == nil {
		for _, l := range jfiles {
			retbytes1, err := os.ReadFile(l)
			if err != nil {
				continue
			}
			_ = json.Unmarshal(retbytes1, &devInfo)

		}
	}
	return devInfo
}

func splitBySize(buf []byte, size int) [][]byte {
	var chunk []byte
	chunks := make([][]byte, 0, len(buf)/size+1)
	for len(buf) >= size {
		chunk, buf = buf[:size], buf[size:]
		chunks = append(chunks, chunk)
	}
	if len(buf) > 0 {
		chunks = append(chunks, buf[:])
	}
	return chunks
}

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
	}
	return false
}

func getFileSha256(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("os open error %v\n", err)
		return nil
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		fmt.Printf("sha256 error %v\n", err)
		return nil
	}
	return h.Sum(nil)
}

func getDevMemStats() {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		fmt.Printf("mem stats error: %v\n", err)
		return
	}
	printColor(" - device memory\n", colorCYAN)
	fmt.Printf("Total = %v MiB\n", bToMb(vmStat.Total))
	fmt.Printf("Available = %v MiB\n", bToMb(vmStat.Available))
	fmt.Printf("Used = %v MiB\n", bToMb(vmStat.Used))
	fmt.Printf("Used Percent = %0.3f\n", vmStat.UsedPercent)
	fmt.Printf("Free = %v MiB\n", bToMb(vmStat.Free))
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
