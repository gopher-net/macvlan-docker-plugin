package macvlan

import (
	"fmt"
	"net"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	sdk "github.com/docker/go-plugins-helpers/network"
	"github.com/samalba/dockerclient"
	"github.com/vishvananda/netlink"
)

const (
	bridgeMode           = "bridge"
	containerIfacePrefix = "eth"
	defaultMTU           = 1500
	minMTU               = 68
)

// Driver is the MACVLAN Driver
type Driver struct {
	sdk.Driver
	dockerer
	networks   networkTable
	nameserver string
	sync.Mutex
}

// NewDriver creates a new MACVLAN Driver
func NewDriver(version string, ctx *cli.Context) (*Driver, error) {
	docker, err := dockerclient.NewDockerClient("unix:///var/run/docker.sock", nil)
	if err != nil {
		return nil, fmt.Errorf("could not connect to docker: %s", err)
	}
	// lower bound of v4 MTU is 68-bytes per rfc791
	if ctx.Int("mtu") <= 0 {
		cliMTU = defaultMTU
	} else if ctx.Int("mtu") >= minMTU {
		cliMTU = ctx.Int("mtu")
	} else {
		log.Fatalf("The MTU value passed [ %d ] must be greater than [ %d ] bytes per rfc791", ctx.Int("mtu"), minMTU)
	}
	// Set the default mode to bridge
	if ctx.String("mode") == "" {
		macvlanMode = bridgeMode
	}
	switch ctx.String("mode") {
	case bridgeMode:
		macvlanMode = bridgeMode
		// todo: in other modes if relevant
	}
	d := &Driver{
		networks: networkTable{},
		dockerer: dockerer{
			client: docker,
		},
	}
	return d, nil
}

// GetCapabilities tells libnetwork this driver is local scope
func (d *Driver) GetCapabilities() (*sdk.CapabilitiesResponse, error) {
	scope := &sdk.CapabilitiesResponse{Scope: sdk.LocalScope}
	return scope, nil
}

// CreateNetwork creates a new MACVLAN network
func (d *Driver) CreateNetwork(r *sdk.CreateNetworkRequest) error {
	var netCidr *net.IPNet
	var netGw string
	var err error
	log.Debugf("Network Create Called: [ %+v ]", r)
	for _, v4 := range r.IPv4Data {
		netGw = v4.Gateway
		_, netCidr, err = net.ParseCIDR(v4.Pool)
		if err != nil {
			return err
		}
	}

	n := &network{
		id:        r.NetworkID,
		endpoints: endpointTable{},
		cidr:      netCidr,
		gateway:   netGw,
	}

	// Parse docker network -o opts
	for k, v := range r.Options {
		if k == "com.docker.sdk.generic" {
			if genericOpts, ok := v.(map[string]interface{}); ok {
				for key, val := range genericOpts {
					log.Debugf("Libnetwork Opts Sent: [ %s ] Value: [ %s ]", key, val)
					// Parse -o host_iface from libnetwork generic opts
					if key == "host_iface" {
						n.ifaceOpt = val.(string)
					}
				}
			}
		}
	}
	d.addNetwork(n)
	return nil
}

// DeleteNetwork deletes a network
func (d *Driver) DeleteNetwork(r *sdk.DeleteNetworkRequest) error {
	log.Debugf("Delete network request: %+v", &r)
	d.deleteNetwork(r.NetworkID)
	return nil
}

// CreateEndpoint creates a new MACVLAN Endpoint
func (d *Driver) CreateEndpoint(r *sdk.CreateEndpointRequest) (*sdk.CreateEndpointResponse, error) {
	endID := r.EndpointID
	log.Debugf("The container subnet for this context is [ %s ]", r.Interface.Address)
	// Request an IP address from libnetwork based on the cidr scope
	// TODO: Add a user defined static ip addr option in Docker v1.10
	containerAddress := r.Interface.Address
	if containerAddress == "" {
		return nil, fmt.Errorf("Unable to obtain an IP address from libnetwork default ipam")
	}
	// generate a mac address for the pending container
	mac := makeMac(net.ParseIP(containerAddress))

	log.Infof("Allocated container IP: [ %s ]", containerAddress)
	// IP addrs comes from libnetwork ipam via user 'docker network' parameters

	res := &sdk.CreateEndpointResponse{
		Interface: &sdk.EndpointInterface{
			Address:    containerAddress,
			MacAddress: mac,
		},
	}
	log.Debugf("Create endpoint response: %+v", res)
	log.Debugf("Create endpoint %s %+v", endID, res)
	return res, nil
}

// DeleteEndpoint deletes a MACVLAN Endpoint
func (d *Driver) DeleteEndpoint(r *sdk.DeleteEndpointRequest) error {
	log.Debugf("Delete endpoint request: %+v", &r)
	//TODO: null check cidr in case driver restarted and doesn't know the network to avoid panic
	log.Debugf("Delete endpoint %s", r.EndpointID)

	containerLink := r.EndpointID[:5]
	// Check the interface to delete exists to avoid a netlink panic
	if ok := validateHostIface(containerLink); !ok {
		return fmt.Errorf("The requested interface to delete [ %s ] was not found on the host.", containerLink)
	}
	// Get the link handle
	link, err := netlink.LinkByName(containerLink)
	if err != nil {
		return fmt.Errorf("Error looking up link [ %s ] object: [ %v ] error: [ %s ]", link.Attrs().Name, link, err)
	}
	log.Infof("Deleting the unused macvlan link [ %s ] from the removed container", link.Attrs().Name)
	if err := netlink.LinkDel(link); err != nil {
		log.Errorf("unable to delete the Macvlan link [ %s ] on leave: %s", link.Attrs().Name, err)
	}
	return nil
}

// EndpointInfo returns informatoin about a MACVLAN endpoint
func (d *Driver) EndpointInfo(r *sdk.InfoRequest) (*sdk.InfoResponse, error) {
	log.Debugf("Endpoint info request: %+v", &r)
	res := &sdk.InfoResponse{
		Value: make(map[string]string),
	}
	return res, nil
}

// Join creates a MACVLAN interface to be moved to the container netns
func (d *Driver) Join(r *sdk.JoinRequest) (*sdk.JoinResponse, error) {
	log.Debugf("Join request: %+v", &r)
	getID, err := d.getNetwork(r.NetworkID)
	if err != nil {
		// Init any existing libnetwork networks
		d.existingNetChecks()

		getID, err = d.getNetwork(r.NetworkID)
		if err != nil {
			return nil, fmt.Errorf("error getting network ID [ %s ]. Run 'docker network ls' or 'docker network create' Err: %v", r.NetworkID, err)
		}
	}
	endID := r.EndpointID
	// unique name while still on the common netns
	preMoveName := endID[:5]
	mode, err := setVlanMode("bridge")
	if err != nil {
		return nil, fmt.Errorf("error getting vlan mode [ %v ]: %s", mode, err)
	}
	if getID.ifaceOpt == "" {
		return nil, fmt.Errorf("Required macvlan parent interface is missing, please recreate the network specifying the -o host_iface=ethX")
	}
	// Get the link for the master index (Example: the docker host eth iface)
	hostEth, err := netlink.LinkByName(getID.ifaceOpt)
	if err != nil {
		log.Warnf("Error looking up the parent iface [ %s ] error: [ %s ]", getID.ifaceOpt, err)
	}
	mvlan := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        preMoveName,
			ParentIndex: hostEth.Attrs().Index,
		},
		Mode: mode,
	}
	if err := netlink.LinkAdd(mvlan); err != nil {
		log.Warnf("Failed to create the netlink link: [ %v ] with the "+
			"error: %s Note: a parent index cannot be link to both macvlan "+
			"and macvlan simultaneously. A new parent index is required", mvlan, err)
		log.Warnf("Also check `/var/run/docker/netns/` for orphaned links to unmount and delete, then restart the plugin")
		log.Warnf("Run this to clean orphaned links 'umount /var/run/docker/netns/* && rm /var/run/docker/netns/*'")
	}
	// Set the netlink iface MTU, default is 1500
	if err := netlink.LinkSetMTU(mvlan, defaultMTU); err != nil {
		log.Errorf("Error setting the MTU [ %d ] for link [ %s ]: %s", defaultMTU, mvlan.Name, err)
	}
	// Bring the netlink iface up
	if err := netlink.LinkSetUp(mvlan); err != nil {
		log.Warnf("failed to enable the macvlan netlink link: [ %v ]", mvlan, err)
	}
	// SrcName gets renamed to DstPrefix on the container iface
	ifname := &sdk.InterfaceName{
		SrcName:   mvlan.Name,
		DstPrefix: containerIfacePrefix,
	}

	res := &sdk.JoinResponse{
		InterfaceName:         *ifname,
		Gateway:               getID.gateway,
		DisableGatewayService: true,
	}
	log.Debugf("Join response: %+v", res)
	log.Debugf("Join endpoint %s:%s to %s", r.NetworkID, r.EndpointID, r.SandboxKey)
	return res, nil
}

// Leave removes a MACVLAN Endpoint from a container
func (d *Driver) Leave(r *sdk.LeaveRequest) error {
	log.Debugf("Leave request: %+v", &r)
	log.Debugf("Leave %s:%s", r.NetworkID, r.EndpointID)
	return nil
}

// DiscoverNew is not used by local scoped drivers
func (d *Driver) DiscoverNew(r *sdk.DiscoveryNotification) error {
	return nil
}

// DiscoverDelete is not used by local scoped drivers
func (d *Driver) DiscoverDelete(r *sdk.DiscoveryNotification) error {
	return nil
}

// existingNetChecks checks for networks that already exist in libnetwork cache
func (d *Driver) existingNetChecks() {
	// Request all networks on the endpoint without any filters
	existingNets, err := d.client.ListNetworks("")
	if err != nil {
		log.Errorf("unable to retrieve existing networks: %v", err)
	}
	var netCidr *net.IPNet
	var netGW string
	for _, n := range existingNets {
		// Exclude the default network names
		if n.Name != "" && n.Name != "none" && n.Name != "host" && n.Name != "bridge" {
			for _, v4 := range n.IPAM.Config {
				netGW = v4.Gateway
				netCidr, err = parseIPNet(v4.Subnet)
				if err != nil {
					log.Errorf("invalid cidr address in network [ %s ]: %v", v4.Subnet, err)
				}
			}
			nw := &network{
				id:        n.ID,
				endpoints: endpointTable{},
				cidr:      netCidr,
				gateway:   netGW,
			}
			// Parse docker network -o opts
			for k, v := range n.Options {
				// Infer a macvlan network from required option
				if k == "host_iface" {
					nw.ifaceOpt = v
					log.Debugf("Existing macvlan network exists: [Name:%s, Cidr:%s, Gateway:%s, Master Iface:%s]",
						n.Name, netCidr.String(), netGW, nw.ifaceOpt)
					d.addNetwork(nw)
				}
			}
		}
	}
}
