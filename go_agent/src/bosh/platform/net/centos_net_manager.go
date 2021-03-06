package net

import (
	"bytes"
	"path/filepath"
	"strings"
	"text/template"

	bosherr "bosh/errors"
	boshlog "bosh/logger"
	bosharp "bosh/platform/net/arp"
	boship "bosh/platform/net/ip"
	boshsettings "bosh/settings"
	boshsys "bosh/system"
)

const centosNetManagerLogTag = "centosNetManager"

type centosNetManager struct {
	DefaultNetworkResolver

	fs                 boshsys.FileSystem
	cmdRunner          boshsys.CmdRunner
	routesSearcher     RoutesSearcher
	ipResolver         boship.IPResolver
	addressBroadcaster bosharp.AddressBroadcaster
	logger             boshlog.Logger
}

func NewCentosNetManager(
	fs boshsys.FileSystem,
	cmdRunner boshsys.CmdRunner,
	defaultNetworkResolver DefaultNetworkResolver,
	ipResolver boship.IPResolver,
	addressBroadcaster bosharp.AddressBroadcaster,
	logger boshlog.Logger,
) centosNetManager {
	return centosNetManager{
		DefaultNetworkResolver: defaultNetworkResolver,
		fs:                 fs,
		cmdRunner:          cmdRunner,
		ipResolver:         ipResolver,
		addressBroadcaster: addressBroadcaster,
		logger:             logger,
	}
}

func (net centosNetManager) getDNSServers(networks boshsettings.Networks) []string {
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")
	return dnsNetwork.DNS
}

func (net centosNetManager) SetupDhcp(networks boshsettings.Networks, errCh chan error) error {
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")

	type dhcpConfigArg struct {
		DNSServers []string
	}

	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("dhcp-config").Parse(centosDHCPConfigTemplate))

	err := t.Execute(buffer, dhcpConfigArg{dnsNetwork.DNS})
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	written, err := net.fs.ConvergeFileContents("/etc/dhcp/dhclient.conf", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/dhcp/dhclient.conf")
	}

	if written {
		net.restartNetwork()
	}

	addresses := []boship.InterfaceAddress{
		// eth0 is hard coded in AWS and OpenStack stemcells.
		// TODO: abstract hardcoded network interface name to the NetManager
		boship.NewResolvingInterfaceAddress("eth0", net.ipResolver),
	}

	go func() {
		net.addressBroadcaster.BroadcastMACAddresses(addresses)
		if errCh != nil {
			errCh <- nil
		}
	}()

	return err
}

// DHCP Config file - /etc/dhcp3/dhclient.conf
const centosDHCPConfigTemplate = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name "<hostname>";

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;

{{ range .DNSServers }}prepend domain-name-servers {{ . }};
{{ end }}`

func (net centosNetManager) SetupManualNetworking(networks boshsettings.Networks, errCh chan error) error {
	modifiedNetworks, err := net.writeIfcfgs(networks)
	if err != nil {
		return bosherr.WrapError(err, "Writing network interfaces")
	}

	net.restartNetwork()

	err = net.writeResolvConf(networks)
	if err != nil {
		return bosherr.WrapError(err, "Writing resolv.conf")
	}

	addresses := toInterfaceAddresses(modifiedNetworks)

	go func() {
		net.addressBroadcaster.BroadcastMACAddresses(addresses)
		if errCh != nil {
			errCh <- nil
		}
	}()

	return nil
}

func (net centosNetManager) writeIfcfgs(networks boshsettings.Networks) ([]customNetwork, error) {
	var modifiedNetworks []customNetwork

	macAddresses, err := net.detectMacAddresses()
	if err != nil {
		return modifiedNetworks, bosherr.WrapError(err, "Detecting mac addresses")
	}

	for _, aNet := range networks {
		var network, broadcast string
		network, broadcast, err = boshsys.CalculateNetworkAndBroadcast(aNet.IP, aNet.Netmask)
		if err != nil {
			return modifiedNetworks, bosherr.WrapError(err, "Calculating network and broadcast")
		}

		newNet := customNetwork{
			aNet,
			macAddresses[aNet.Mac],
			network,
			broadcast,
			true,
		}
		modifiedNetworks = append(modifiedNetworks, newNet)

		buffer := bytes.NewBuffer([]byte{})
		t := template.Must(template.New("ifcfg").Parse(centosIfcgfTemplate))

		err = t.Execute(buffer, newNet)
		if err != nil {
			return modifiedNetworks, bosherr.WrapError(err, "Generating config from template")
		}

		err = net.fs.WriteFile(filepath.Join("/etc/sysconfig/network-scripts", "ifcfg-"+newNet.Interface), buffer.Bytes())
		if err != nil {
			return modifiedNetworks, bosherr.WrapError(err, "Writing to /etc/sysconfig/network-scripts")
		}
	}

	return modifiedNetworks, nil
}

const centosIfcgfTemplate = `DEVICE={{ .Interface }}
BOOTPROTO=static
IPADDR={{ .IP }}
NETMASK={{ .Netmask }}
BROADCAST={{ .Broadcast }}
{{ if .HasDefaultGateway }}GATEWAY={{ .Gateway }}{{ end }}
ONBOOT=yes`

func (net centosNetManager) writeResolvConf(networks boshsettings.Networks) error {
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("resolv-conf").Parse(centosResolvConfTemplate))

	dnsServers := net.getDNSServers(networks)
	dnsServersArg := dnsConfigArg{dnsServers}
	err := t.Execute(buffer, dnsServersArg)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	err = net.fs.WriteFile("/etc/resolv.conf", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/resolv.conf")
	}

	return nil
}

const centosResolvConfTemplate = `# Generated by bosh-agent
{{ range .DNSServers }}nameserver {{ . }}
{{ end }}`

func (net centosNetManager) detectMacAddresses() (map[string]string, error) {
	addresses := map[string]string{}

	filePaths, err := net.fs.Glob("/sys/class/net/*")
	if err != nil {
		return addresses, bosherr.WrapError(err, "Getting file list from /sys/class/net")
	}

	var macAddress string
	for _, filePath := range filePaths {
		macAddress, err = net.fs.ReadFileString(filepath.Join(filePath, "address"))
		if err != nil {
			return addresses, bosherr.WrapError(err, "Reading mac address from file")
		}

		macAddress = strings.Trim(macAddress, "\n")

		interfaceName := filepath.Base(filePath)
		addresses[macAddress] = interfaceName
	}

	return addresses, nil
}

func (net centosNetManager) restartNetwork() {
	_, _, _, err := net.cmdRunner.RunCommand("service", "network", "restart")
	if err != nil {
		net.logger.Info(centosNetManagerLogTag, "Ignoring network restart failure: %#v", err)
	}
}
