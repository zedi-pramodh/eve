package zedkube

import (
	"fmt"
	"strings"

	"github.com/lf-edge/eve/pkg/pillar/types"
)

const (
	defaultCNINamespace  = "kube-system"
	eveNamespace         = "eve-kube-app"
	defaultLocalNIPrefix = "defaultlocal"
)

func genNISpecCreate(ctx *zedkubeContext, niStatus *types.NetworkInstanceStatus) error {
	var err error
	switch niStatus.Type {
	case types.NetworkInstanceTypeSwitch:
		err = switchNISpecCreate(ctx, niStatus)
	case types.NetworkInstanceTypeLocal:
		err = localNISpecCreate(ctx, niStatus)
	default:
		err = fmt.Errorf("genNISpecCreate: NI type %v not supported", niStatus.Type)
	}

	return err
}

func switchNISpecCreate(ctx *zedkubeContext, niStatus *types.NetworkInstanceStatus) error {
	niUUID := niStatus.UUID
	// FC 1123 subdomain must consist of lower case alphanumeric characters
	name := strings.ToLower(niStatus.DisplayName)
	namespace := eveNamespace

	status, err := kubeGetNIStatus(ctx, niUUID)
	if err != nil || status.BridgeName == "" {
		log.Noticef("localNISpecCreate: spec get status wait. status %+v, err %v", status, err)
		return err
	}

	pluginName := "bridge-" + status.BridgeName
	pluginBridge := status.BridgeName
	macAddress := status.BridgeMac

	// Create the config string for net-attach-def
	output := fmt.Sprintf(" {\n")
	output = output + fmt.Sprintf(`    "cniVersion": "0.3.1",
    "plugins": [
      {
        "name": "%s",
        "type": "bridge",
        "bridge": "%s",
        "isDefaultGateway": false,
        "ipMasq": false,
        "hairpinMode": true,
        "mac": "%s",
        "ipam": {
          "type": "dhcp"
        }
      },
      {
        "capabilities": { "mac": true, "ips": true },
        "type": "tuning"
      },
      {
        "type": "eve-bridge"
      }
    ]
`, pluginName, pluginBridge, macAddress)
	output = output + fmt.Sprintf("  }\n")

	err = sendToApiServer(ctx, []byte(output), name, namespace)
	log.Noticef("switch2NISpecCreate: spec, sendToApiServer, error %v", err)
	return err
}

func localNISpecCreate(ctx *zedkubeContext, niStatus *types.NetworkInstanceStatus) error {

	niUUID := niStatus.UUID
	// FC 1123 subdomain must consist of lower case alphanumeric characters
	name := strings.ToLower(niStatus.DisplayName)
	namespace := eveNamespace

	status, err := kubeGetNIStatus(ctx, niUUID)
	if err != nil || status.BridgeName == "" {
		log.Noticef("localNISpecCreate: spec get status wait. status %+v, err %v", status, err)
		return err
	}

	pluginName := "bridge-" + status.BridgeName
	pluginBridge := status.BridgeName
	macAddress := status.BridgeMac
	subnet := niStatus.Subnet.String()
	rangeStart := niStatus.DhcpRange.Start.String()
	rangeEnd := niStatus.DhcpRange.End.String()
	gateway := niStatus.Gateway.String()
	route1 := "10.1.0.0/16"
	route2 := "192.168.86.0/24"
	nameserver1 := "8.8.8.8"
	nameserver2 := "1.1.1.1"
	port := niStatus.PortLogicalLabel

	// Create the config string for net-attach-def
	output := fmt.Sprintf(" {\n")
	output = output + fmt.Sprintf(`    "cniVersion": "0.3.1",
    "plugins": [
      {
        "name": "%s",
        "type": "bridge",
        "bridge": "%s",
        "isDefaultGateway": true,
        "ipMasq": true,
        "hairpinMode": true,
        "mac": "%s",
        "ipam": {
          "type": "host-local",
          "ranges": [
            [
              {
                "subnet": "%s",
                "rangeStart": "%s",
                "rangeEnd": "%s",
                "gateway": "%s"
              }
            ]
          ],
          "routes": [
            { "dst": "%s" },
            { "dst": "%s" }
          ],
          "dns": {
            "nameservers": [ "%s", "%s" ]
          }
        }
      },
      {
        "capabilities": { "mac": true, "ips": true },
        "type": "tuning"
      },
      {
        "port": "%s",
        "type": "eve-bridge"
      }
    ]
`, pluginName, pluginBridge, macAddress, subnet, rangeStart,
		rangeEnd, gateway, route1, route2, nameserver1, nameserver2, port)
	output = output + fmt.Sprintf("  }\n")

	err = sendToApiServer(ctx, []byte(output), name, namespace)
	log.Noticef("localNISpecCreate: spec, sendToApiServer, error %v", err)
	return err
}
