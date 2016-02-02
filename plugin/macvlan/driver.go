package macvlan

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"github.com/docker/libnetwork/driverapi"
	"github.com/gorilla/mux"
	"github.com/samalba/dockerclient"
	"github.com/vishvananda/netlink"
)

const (
	MethodReceiver       = "NetworkDriver"
	bridgeMode           = "bridge"
	containerIfacePrefix = "eth"
	defaultMTU           = 1500
	minMTU               = 68
)

type Driver interface {
	Listen(string) error
}

type driver struct {
	dockerer
	networks   networkTable
	nameserver string
	sync.Mutex
}

type endpoint struct {
	id      string
	mac     net.HardwareAddr
	addr    *net.IPNet
	srcName string
}

type endpointTable map[string]*endpoint

func New(version string, ctx *cli.Context) (Driver, error) {
	docker, err := dockerclient.NewDockerClient("unix:///var/run/docker.sock", nil)
	if err != nil {
		return nil, fmt.Errorf("could not connect to docker: %s", err)
	}
	// lower bound of v4 MTU is 68-bytes per rfc791
	if ctx.Int("mtu") <= 0 {
		cliMTU = cliMTU
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
	d := &driver{
		networks: networkTable{},
		dockerer: dockerer{
			client: docker,
		},
	}
	return d, nil
}

func (driver *driver) Listen(socket string) error {
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(notFound)

	router.Methods("POST").Path("/Plugin.Activate").HandlerFunc(driver.handshake)
	router.Methods("POST").Path("/NetworkDriver.GetCapabilities").HandlerFunc(driver.capabilities)

	handleMethod := func(method string, h http.HandlerFunc) {
		router.Methods("POST").Path(fmt.Sprintf("/%s.%s", MethodReceiver, method)).HandlerFunc(h)
	}
	handleMethod("CreateNetwork", driver.createNetwork)
	handleMethod("DeleteNetwork", driver.deleteNetwork)
	handleMethod("CreateEndpoint", driver.createEndpoint)
	handleMethod("DeleteEndpoint", driver.deleteEndpoint)
	handleMethod("EndpointOperInfo", driver.infoEndpoint)
	handleMethod("Join", driver.joinEndpoint)
	handleMethod("Leave", driver.leaveEndpoint)
	var (
		listener net.Listener
		err      error
	)

	listener, err = net.Listen("unix", socket)
	if err != nil {
		return err
	}

	return http.Serve(listener, router)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	log.Warnf("plugin Not found: [ %+v ]", r)
	http.NotFound(w, r)
}

func sendError(w http.ResponseWriter, msg string, code int) {
	log.Errorf("%d %s", code, msg)
	http.Error(w, msg, code)
}

func errorResponsef(w http.ResponseWriter, fmtString string, item ...interface{}) {
	json.NewEncoder(w).Encode(map[string]string{
		"Err": fmt.Sprintf(fmtString, item...),
	})
}

func objectResponse(w http.ResponseWriter, obj interface{}) {
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		sendError(w, "Could not JSON encode response", http.StatusInternalServerError)
		return
	}
}

func emptyResponse(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]string{})
}

type handshakeResp struct {
	Implements []string
}

func (driver *driver) handshake(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&handshakeResp{
		[]string{"NetworkDriver"},
	})
	if err != nil {
		log.Fatalf("handshake encode: %s", err)
		sendError(w, "encode error", http.StatusInternalServerError)
		return
	}
	log.Debug("Handshake completed")
}

type capabilitiesResp struct {
	Scope string
}

func (driver *driver) capabilities(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&capabilitiesResp{
		"local",
	})
	if err != nil {
		log.Fatalf("capabilities encode: %s", err)
		sendError(w, "encode error", http.StatusInternalServerError)
		return
	}
	log.Debug("Capabilities exchange complete")
}

type networkCreate struct {
	NetworkID string
	Options   map[string]interface{}
	IpV4Data  []driverapi.IPAMData
	ipV6Data  []driverapi.IPAMData
}

func (driver *driver) createNetwork(w http.ResponseWriter, r *http.Request) {
	var create networkCreate
	err := json.NewDecoder(r.Body).Decode(&create)
	if err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	var netCidr *net.IPNet
	var netGw string
	log.Debugf("Network Create Called: [ %+v ]", create)
	for _, v4 := range create.IpV4Data {
		netGw = v4.Gateway.IP.String()
		netCidr = v4.Pool
	}
	n := &network{
		id:        create.NetworkID,
		endpoints: endpointTable{},
		cidr:      netCidr,
		gateway:   netGw,
	}
	// Parse docker network -o opts
	for k, v := range create.Options {
		if k == "com.docker.network.generic" {
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
	driver.addNetwork(n)
	emptyResponse(w)
}

type networkDelete struct {
	NetworkID string
}

func (driver *driver) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	var delete networkDelete
	nid := delete.NetworkID
	if err := json.NewDecoder(r.Body).Decode(&delete); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Debugf("Delete network request: %+v", &delete)
	driver.delNetwork(nid)
}

type endpointCreate struct {
	NetworkID  string
	EndpointID string
	Interface  *EndpointInterface
	Options    map[string]interface{}
}

// EndpointInterface represents an interface endpoint.
type EndpointInterface struct {
	Address     string
	AddressIPv6 string
	MacAddress  string
}

type InterfaceName struct {
	SrcName   string
	DstName   string
	DstPrefix string
}

type endpointResponse struct {
	Interface EndpointInterface
}

func (driver *driver) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var create endpointCreate
	if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	endID := create.EndpointID
	log.Debugf("The container subnet for this context is [ %s ]", create.Interface.Address)
	// Request an IP address from libnetwork based on the cidr scope
	// TODO: Add a user defined static ip addr option in Docker v1.10
	containerAddress := create.Interface.Address
	if containerAddress == "" {
		log.Errorf("Unable to obtain an IP address from libnetwork default ipam")
		return
	}
	// generate a mac address for the pending container
	mac := makeMac(net.ParseIP(containerAddress))

	log.Infof("Allocated container IP: [ %s ]", containerAddress)
	// IP addrs comes from libnetwork ipam via user 'docker network' parameters
	respIface := &EndpointInterface{
		MacAddress: mac,
	}
	resp := &endpointResponse{
		Interface: *respIface,
	}
	log.Debugf("Create endpoint response: %+v", resp)
	objectResponse(w, resp)
	log.Debugf("Create endpoint %s %+v", endID, resp)
}

type endpointDelete struct {
	NetworkID  string
	EndpointID string
}

func (driver *driver) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	var delete endpointDelete
	if err := json.NewDecoder(r.Body).Decode(&delete); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Delete endpoint request: %+v", &delete)
	emptyResponse(w)
	//TODO: null check cidr in case driver restarted and doesn't know the network to avoid panic
	log.Debugf("Delete endpoint %s", delete.EndpointID)

	containerLink := delete.EndpointID[:5]
	// Check the interface to delete exists to avoid a netlink panic
	if ok := validateHostIface(containerLink); !ok {
		log.Errorf("The requested interface to delete [ %s ] was not found on the host.", containerLink)
		return
	}
	// Get the link handle
	link, err := netlink.LinkByName(containerLink)
	if err != nil {
		log.Errorf("Error looking up link [ %s ] object: [ %v ] error: [ %s ]", link.Attrs().Name, link, err)
		return
	}
	log.Infof("Deleting the unused macvlan link [ %s ] from the removed container", link.Attrs().Name)
	if err := netlink.LinkDel(link); err != nil {
		log.Errorf("unable to delete the Macvlan link [ %s ] on leave: %s", link.Attrs().Name, err)
	}
}

type endpointInfoReq struct {
	NetworkID  string
	EndpointID string
}

type endpointInfo struct {
	Value map[string]interface{}
}

func (driver *driver) infoEndpoint(w http.ResponseWriter, r *http.Request) {
	var info endpointInfoReq
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Endpoint info request: %+v", &info)
	objectResponse(w, &endpointInfo{Value: map[string]interface{}{}})
	log.Debugf("Endpoint info %s", info.EndpointID)
}

type joinInfo struct {
	InterfaceName *InterfaceName
	Gateway       string
	GatewayIPv6   string
}

type join struct {
	NetworkID  string
	EndpointID string
	SandboxKey string
	Options    map[string]interface{}
}

type staticRoute struct {
	Destination string
	RouteType   int
	NextHop     string
}

type joinResponse struct {
	Gateway               string
	InterfaceName         InterfaceName
	StaticRoutes          []*staticRoute
	DisableGatewayService bool
}

func (driver *driver) joinEndpoint(w http.ResponseWriter, r *http.Request) {

	var j join
	if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Join request: %+v", &j)
	getID, err := driver.getNetwork(j.NetworkID)
	if err != nil {
		log.Errorf("error getting network ID mode [ %s ]: %v", j.NetworkID, err)
	}
	endID := j.EndpointID
	// unique name while still on the common netns
	preMoveName := endID[:5]
	res := &joinResponse{}
	mode, err := setVlanMode("bridge")
	if err != nil {
		log.Errorf("error getting vlan mode [ %v ]: %s", mode, err)
		return
	}
	if getID.ifaceOpt == "" {
		log.Error("Required macvlan parent interface is missing, please recreate the network specifying the host_iface")
		return
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
	ifname := &InterfaceName{
		SrcName:   mvlan.Name,
		DstPrefix: containerIfacePrefix,
	}

	res = &joinResponse{
		InterfaceName:         *ifname,
		Gateway:               getID.gateway,
		DisableGatewayService: true,
	}
	defer objectResponse(w, res)
	log.Debugf("Join response: %+v", res)
	// Send the response to libnetwork
	//	objectResponse(w, res)
	log.Debugf("Join endpoint %s:%s to %s", j.NetworkID, j.EndpointID, j.SandboxKey)
}

type leave struct {
	NetworkID  string
	EndpointID string
	Options    map[string]interface{}
}

func (driver *driver) leaveEndpoint(w http.ResponseWriter, r *http.Request) {
	var l leave
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Leave request: %+v", &l)
	emptyResponse(w)
	log.Debugf("Leave %s:%s", l.NetworkID, l.EndpointID)
}
