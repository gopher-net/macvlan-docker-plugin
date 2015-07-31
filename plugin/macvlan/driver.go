package macvlan

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/ipallocator"
	"github.com/docker/libnetwork/types"
	"github.com/gorilla/mux"
	"github.com/samalba/dockerclient"
	"github.com/vishvananda/netlink"
)

const (
	MethodReceiver       = "NetworkDriver"
	defaultRoute         = "0.0.0.0/0"
	containerIfacePrefix = "eth"
)

type Driver interface {
	Listen(string) error
}

// Struct for binding bridge options CLI flags
type bridgeOpts struct {
	brName   string
	brSubnet net.IPNet
	brIP     net.IPNet
}

type driver struct {
	dockerer
	ipAllocator *ipallocator.IPAllocator
	version     string
	network     string
	cidr        *net.IPNet
	nameserver  string
}

func New(version string) (Driver, error) {
	docker, err := dockerclient.NewDockerClient("unix:///var/run/docker.sock", nil)
	if err != nil {
		return nil, fmt.Errorf("could not connect to docker: %s", err)
	}

	ipAllocator := ipallocator.New()
	d := &driver{
		dockerer: dockerer{
			client: docker,
		},
		ipAllocator: ipAllocator,
		version:     version,
	}
	return d, nil
}

func (driver *driver) Listen(socket string) error {
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(notFound)

	router.Methods("GET").Path("/status").HandlerFunc(driver.status)
	router.Methods("POST").Path("/Plugin.Activate").HandlerFunc(driver.handshake)

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
	log.Warnf("[plugin] Not found: %+v", r)
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

func (driver *driver) status(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, fmt.Sprintln("macvlan plugin", driver.version))
}

type networkCreate struct {
	NetworkID string
	Options   map[string]interface{}
}

func (driver *driver) createNetwork(w http.ResponseWriter, r *http.Request) {
	var create networkCreate
	err := json.NewDecoder(r.Body).Decode(&create)
	if err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if driver.network != "" {
		errorResponsef(w, "You get just one network, and you already made %s", driver.network)
		return
	}
	driver.network = create.NetworkID

	// Parse the network address from the user supplied or default container network
	_, ipNet, err := net.ParseCIDR(defaultSubnet)
	if err != nil {
		log.Warnf("Error parsing cidr from the default subnet: %s", err)
	}
	//	cidr := ipNet
	driver.cidr = ipNet
	driver.ipAllocator.RequestIP(driver.cidr, nil)

	emptyResponse(w)
}

type networkDelete struct {
	NetworkID string
}

func (driver *driver) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	var delete networkDelete
	if err := json.NewDecoder(r.Body).Decode(&delete); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Debugf("Delete network request: %+v", &delete)
	if delete.NetworkID != driver.network {
		log.Debugf("network not found: %+v", &delete)
		errorResponsef(w, "Network %s not found", delete.NetworkID)
		return
	}
	driver.network = ""
	emptyResponse(w)
	log.Infof("Destroy network %s", delete.NetworkID)
}

type endpointCreate struct {
	NetworkID  string
	EndpointID string
	Interfaces []*iface
	Options    map[string]interface{}
}

type iface struct {
	ID         int
	SrcName    string
	DstPrefix  string
	Address    string
	MacAddress string
}

type endpointResponse struct {
	Interfaces []*iface
}

func (driver *driver) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var create endpointCreate
	if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	netID := create.NetworkID
	endID := create.EndpointID

	if netID != driver.network {
		log.Warnf("Network not found, [ %s ]", netID)
		errorResponsef(w, "No such network %s", netID)
		return
	}
	// Request an IP address from libnetwork based on the cidr scope
	// TODO: Add a user defined static ip addr option
	allocatedIP, err := driver.ipAllocator.RequestIP(driver.cidr, nil)
	if err != nil || allocatedIP == nil {
		log.Errorf("Unable to obtain an IP address from libnetwork ipam: %s", err)
		errorResponsef(w, "%s", err)
		return
	}
	// generate a mac address for the pending container
	mac := makeMac(allocatedIP)
	// Have to convert container IP to a string ip/mask format
	_, containerMask := driver.cidr.Mask.Size()
	containerAddress := allocatedIP.String() + "/" + strconv.Itoa(containerMask)
	log.Infof("The allocated container IP is: [ %s ]", allocatedIP.String())

	respIface := &iface{
		Address:    containerAddress,
		MacAddress: mac,
	}
	resp := &endpointResponse{
		Interfaces: []*iface{respIface},
	}
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
	// null check cidr in case driver restarted and doesnt know the network to avoid panic
	if driver.cidr == nil {
		return
	}
	// ReleaseIP releases an ip back to a network
	if err := driver.ipAllocator.ReleaseIP(driver.cidr, driver.cidr.IP); err != nil {
		log.Warnf("error releasing IP: %s", err)
	}
	log.Debugf("Delete endpoint %s", delete.EndpointID)

	containerLink := delete.EndpointID[:5]

	log.Infof("Removing unused macvlan link [ %s ] from the removed container", containerLink)
	clink, err := netlink.LinkByName(containerLink)
	if err != nil {
		log.Warnf("Error looking up link [ %s ] object: [ %v ] error: [ %s ]", clink.Attrs().Name, clink, err)
	}
	if err := netlink.LinkDel(clink); err != nil {
		log.Errorf("unable to delete the Macvlan link [ %s ] on leave: %s", clink, err)
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
	InterfaceNames []*iface
	Gateway        string
	GatewayIPv6    string
	HostsPath      string
	ResolvConfPath string
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
	InterfaceID int
}

type joinResponse struct {
	HostsPath      string
	ResolvConfPath string
	Gateway        string
	InterfaceNames []*iface
	StaticRoutes   []*staticRoute
}

func (driver *driver) joinEndpoint(w http.ResponseWriter, r *http.Request) {
	var j join
	if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Join request: %+v", &j)

	endID := j.EndpointID
	// unique name while still on the common netns
	preMoveName := endID[:5]
	mode, err := setVlanMode(macvlanMode)
	if err != nil {
		log.Errorf("%s", err)
		return
	}
	m, err := netlink.LinkByName(macvlanEthIface)
	if err != nil {
		log.Warnf("Error looking up the parent iface [ %s ] mode: [ %s ] error: [ %s ]", macvlanEthIface, mode, err)
	}
	macvlan := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        preMoveName,
			ParentIndex: m.Attrs().Index,
		},
		Mode: mode,
	}
	if err := netlink.LinkAdd(macvlan); err != nil {
		log.Warnf("failed to create Macvlan: [ %v ] with the error: %s", macvlan, err)
	}
	log.Infof("Created Macvlan port: [ %s ] using the mode: [ %s ]", macvlan.Name, macvlanMode)

	// SrcName gets renamed to DstPrefix on the container iface
	ifname := &iface{
		SrcName:   macvlan.Name,
		DstPrefix: containerIfacePrefix,
		ID:        0,
	}
	res := &joinResponse{
		InterfaceNames: []*iface{ifname},
		Gateway:        gatewayIP,
	}
	// Add Connected Route
	routeToDNS := &staticRoute{
		Destination: defaultSubnet,
		RouteType:   types.CONNECTED,
		NextHop:     "",
		InterfaceID: 0,
	}
	res.StaticRoutes = []*staticRoute{routeToDNS}

	objectResponse(w, res)
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
