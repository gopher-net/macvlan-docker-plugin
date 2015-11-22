ipvlan-docker-plugin
=================

This repo is for examples of a plugin w/ libnetwork and temporary until we get Ipvlan/Macvlan native driver support in Docker which will be
relatively soon. This will deprecate in order to focus on much more interesting Gopher networking scenarios like integrations. While it can be supported here, the effort would be kind of wasted since we will get it native soon so
hang tight for trunks.


Macvlan is a lightweight bridgless implementation that can be ideal for some scenarios that fit simple network needs with vlans and 802.1q trunks.

### Pre-Requisites

#### Kernel Dependencies

The kernel dependency is the macvlan kernel module support. You can verify if you have it compiled in or not with the following:

```
$ modprobe macvlan
$ lsmod | grep macvlan
  macvlan   24576  0
```
If you get any errors or it doesn't show up from `lsmod` then you probably need to simply upgrade the kernel version. Here is an example upgrade to `v4.3-rc2` that works on both Ubuntu 15.04 and 14.10 along with the similar Debian distributions.

```
$ wget http://kernel.ubuntu.com/~kernel-ppa/mainline/v4.3-rc2-unstable/linux-headers-4.3.0-040300rc2_4.3.0-040300rc2.201509201830_all.deb
$ wget http://kernel.ubuntu.com/~kernel-ppa/mainline/v4.3-rc2-unstable/linux-headers-4.3.0-040300rc2-generic_4.3.0-040300rc2.201509201830_amd64.deb
$ wget http://kernel.ubuntu.com/~kernel-ppa/mainline/v4.3-rc2-unstable/linux-image-4.3.0-040300rc2-generic_4.3.0-040300rc2.201509201830_amd64.deb
$ dpkg -i linux-headers-4.3*.deb linux-image-4.3*.deb
$ reboot
```

As of Docker v1.9 the docker/libnetwork APIs are packaged by default in Docker. Grab the latest or v1.9+ version of Docker from [Latest Linux
binary from Docker](http://docs.docker.com/engine/installation/binaries/). Alternatively `curl -sSL https://get.docker.com/ | sh` or from your
distribution repo or docker repos.

### Macvlan Bridge Mode Instructions

**1.** Start Docker with the following or simply start the service. Version 1.9+ is required.

```
$ docker -v
Docker version 1.9.1, build a34a1d5

# -D is optional debugging
$ docker daemon -D
```

**2.**  Start the driver in bridge mode.

- *Note:* The host-interface flag is the Docker host's eth interface. It shouldnt be required for the driver but this is a bit lazy and isnt
honoring docker network opts yet.

In the repo directory, use the binary named `macvlan-docker-plugin-0.2-Linux-x86_64`. Feel free to rename it :)

```
$ git clone https://github.com/gopher-net/macvlan-docker-plugin.git
$ cd macvlan-docker-plugin/binaries
$ ./macvlan-docker-plugin-0.2-Linux-x86_64 -d --host-interface=eth1 --mode=bridge

# -d is debug
# --host-interface is the master interface, eth0, eth1 etc. The docker network create needs to correspond to that subnet for bridge mode
```

**3.** Create a network with Docker

**Note** the subnet needs to correspond to the master interface.  In this example, the nic `eth1` is attached to a subnet `192.168.1.0/24`. The container needs to be on the same *broadast domain* as the default gateway. In this case it is a router with the address of `192.168.1.1`.

*TODO is to honor the Docker opts that will enable multiple master interfaces for
subinterfaces and other ethX interfaces at once. Opts from Docker network are parsed, just not honored. Adding that to a native driver into
libnetwork will be easier and more likely then anyone doing it here.*

```
$ docker network create -d macvlan --subnet=192.168.1.0/24 --gateway=192.168.1.1 -o host_iface=eth1 net1
```

**4.** Run some containers on the new network

 Run some containers, specify the network and verify they can ping one another

```
$ docker run --net=net1 -it --rm ubuntu
```

Docker networks are now persistant after a reboot. To remove all of the network configs on a docker daemon restart you can simply delete the directory with: `rm  /var/lib/docker/network/files/*`

### Notes and General Macvlan Caveats

- There can only be one network type bound to the host interface at any given time. Example: Macvlan Bridge or IPVlan L2. There is no mixing.
- The specified gateway is external to the host or at least not defined by the driver itself.
- Multiple drivers can be active at any time. However, Macvlan and Ipvlan are not compatable on the same master interface (e.g. eth0).
- You can create multiple networks and have active containers in each network as long as they are all of the same mode type.
- Each network is isolated from one another. Any container inside the network/subnet can talk to one another without a reachable gateway.
- Containers on separate networks cannot reach one another without an external process routing between the two networks/subnets.


### Dev and issues

To run the plugin via Go for hacking simply run go with the `main.go` entry point and desired parameters. The same applies to the [gopher-net/ipvlan](https://github.com/gopher-net/ipvlan-docker-plugin) driver:

```
go run main.go -d --host-interface=eth1 --mode=bridge
```

Use [Godep](https://github.com/tools/godep) for dependencies.

Install and use Godep with the following:

```
$ go get github.com/tools/godep
# From inside the plugin directory where the Godep directory is restore the snapshotted dependencies used by libnetwork:
$ godep restore
```

 There is a `godbus/dbus` version that conflicts with `vishvananda/netlink` that will lead to this error at build time. This can appear as libnetwork issues when in fact it is 3rd party drivers. Libnetwork also uses Godep for versioning so using those versions would be just as good or even better if keeping with the latest experimental nightly Docker builds:

Example of the godbus error:

```
../../../docker/libnetwork/iptables/firewalld.go:75: cannot use c.sysconn.Object(dbusInterface, dbus.ObjectPath(dbusPath)) (type dbus.BusObject) as type *dbus.
Object in assignment: need type assertion
```

- If you dont want to use godep @Orivej graciously pointed out the godbus dependency in issue #5:

"You need a stable godbus that you can probably get with:"
```
cd $GOPATH/src/github.com/godbus/dbus
git checkout v2
```

 - Another option would be to use godep and sync your library with libnetworks.

```
go get github.com/tools/godep
git clone https://github.com/docker/libnetwork.git
cd libnetwork
godep restore
```