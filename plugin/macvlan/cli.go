package macvlan

import "github.com/codegangsta/cli"

// Exported Flag Opts
var (
	// FlagMacvlanMode TODO: Values need to be bound to driver. Need to modify the Driver iface. Added brOpts if we want to pass that to Listen(string)
	FlagMacvlanMode  = cli.StringFlag{Name: "macvlan-mode", Value: macvlanMode, Usage: "name of the macvlan mode [bridge|private|passthrough|vepa]. By default, bridge mode is implicit: --bridge-name=bridge"}
	FlagGateway      = cli.StringFlag{Name: "gateway", Value: gatewayIP, Usage: "IP of the default gateway. default: --bridge-ip=172.18.40.1/24"}
	FlagBridgeSubnet = cli.StringFlag{Name: "macvlan-subnet", Value: defaultSubnet, Usage: "subnet for the containers (currently IPv4 support)"}
	FlagMacvlanEth   = cli.StringFlag{Name: "macvlan-interface", Value: macvlanEthIface, Usage: "subnet for the containers (currently IPv4 support)"}
)

// Unexported variables
var (
	// TODO: Temp hardcodes, bind to CLI flags and/or dnet-ctl for bridge properties.
	macvlanMode     = "bridge"         // should this be the default mode?
	macvlanEthIface = "eth1"           // default to eth0?
	defaultSubnet   = "192.168.1.0/24" // Should this just be the eth0 IP subnet?
	gatewayIP       = "192.168.1.1"    // Should this just be the eth0 IP addr?
)
