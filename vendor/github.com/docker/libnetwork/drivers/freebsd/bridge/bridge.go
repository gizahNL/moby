// +build freebsd

package bridge

import (
	//"bufio"
	"errors"
	"fmt"
	"net"
	//"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/discoverapi"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/netutils"
	"github.com/docker/libnetwork/options"
	//"github.com/docker/libnetwork/portmapper" // ==> iptables ==> netlink
	"github.com/docker/libnetwork/types"
)

const (
	networkType = "bridge"

	// DefaultBridgeName is the default name for the bridge interface managed
	// by the driver when unspecified by the caller.
	DefaultBridgeName = "docker0"

	// BridgeName label for bridge driver
	BridgeName = "com.docker.network.bridge.name"

	// EnableIPMasquerade label for bridge driver
	EnableIPMasquerade = "com.docker.network.bridge.enable_ip_masquerade"

	// EnableICC label
	EnableICC = "com.docker.network.bridge.enable_icc"

	// DefaultBindingIP label
	DefaultBindingIP = "com.docker.network.bridge.host_binding_ipv4"

	// DefaultBridge label
	DefaultBridge = "com.docker.network.bridge.default_bridge"

	// DefaultGatewayV4AuxKey represents the default-gateway configured by the user
	DefaultGatewayV4AuxKey = "DefaultGatewayIPv4"

	// DefaultGatewayV6AuxKey represents the ipv6 default-gateway configured by the user
	DefaultGatewayV6AuxKey = "DefaultGatewayIPv6"
        vethPrefix                 = "veth"
        vethLen                    = 7
        defaultContainerVethPrefix = "eth"
)

// configuration info for the "bridge" driver.
type configuration struct {
	EnableIPForwarding  bool
	EnableIPTables      bool
	EnableUserlandProxy bool
}

// networkConfiguration for network specific configuration
type networkConfiguration struct {
	ID                 string
	BridgeName         string
	BridgeNameInternal string
	EnableIPv6         bool
	EnableIPMasquerade bool
	EnableICC          bool
        InhibitIPv4          bool
	Mtu                int
	DefaultBindingIntf string
	DefaultBindingIP   net.IP
	DefaultBridge      bool
        HostIP               net.IP
        ContainerIfacePrefix string
	// Internal fields set after ipam data parsing
	AddressIPv4        *net.IPNet
	AddressIPv6        *net.IPNet
	DefaultGatewayIPv4 net.IP
	DefaultGatewayIPv6 net.IP
	dbIndex            uint64
	dbExists           bool
	Internal           bool
        BridgeIfaceCreator ifaceCreator
}

// ifaceCreator represents how the bridge interface was created
type ifaceCreator int8

const (
        ifaceCreatorUnknown ifaceCreator = iota
        ifaceCreatedByLibnetwork
        ifaceCreatedByUser
)


// endpointConfiguration represents the user specified configuration for the sandbox endpoint
type endpointConfiguration struct {
	MacAddress   net.HardwareAddr
	PortBindings []types.PortBinding
	ExposedPorts []types.TransportPort
	containerID  string
}

// containerConfiguration represents the user specified configuration for a container
type containerConfiguration struct {
	ParentEndpoints []string
	ChildEndpoints  []string
}

// cnnectivityConfiguration represents the user specified configuration regarding the external connectivity
type connectivityConfiguration struct {
	PortBindings []types.PortBinding
	ExposedPorts []types.TransportPort
}

type bridgeEndpoint struct {
	id              string
	nid             string
	srcName         string
	addr            *net.IPNet
	addrv6          *net.IPNet
	macAddress      net.HardwareAddr
	config          *endpointConfiguration // User specified parameters
	containerConfig *containerConfiguration
	extConnConfig   *connectivityConfiguration
	portMapping     []types.PortBinding // Operation port bindings
	dbIndex         uint64
	dbExists        bool
}

type bridgeInterface struct {
	Link	    *net.Interface
	bridgeIPv4  *net.IPNet
	bridgeIPv6  *net.IPNet
	gatewayIPv4 net.IP
	gatewayIPv6 net.IP
}

type bridgeNetwork struct {
	id         string
	bridge     *bridgeInterface
	config     *networkConfiguration
	endpoints  map[string]*bridgeEndpoint // key: endpoint id
	//portMapper *portmapper.PortMapper
	driver     *driver // The network's driver
	sync.Mutex
}

type driver struct {
	config         *configuration
	network        *bridgeNetwork
	//natChain       *iptables.ChainInfo
	//filterChain    *iptables.ChainInfo
	//isolationChain *iptables.ChainInfo
	networks       map[string]*bridgeNetwork
	store          datastore.DataStore
	defrouteIP net.IP
        configNetwork     sync.Mutex
        sync.Mutex
}

// New constructs a new bridge driver
func newDriver() *driver {
	return &driver{networks: map[string]*bridgeNetwork{}}
}

// Init registers a new instance of bridge driver
func Init(dc driverapi.DriverCallback, config map[string]interface{}) error {
	d := newDriver()
	if err := d.configure(config); err != nil {
		return err
	}

	c := driverapi.Capability{
		DataScope: datastore.LocalScope,
	}
	return dc.RegisterDriver(networkType, d, c)
}

func (d *driver) NetworkAllocate(id string, option map[string]string, ipV4Data, ipV6Data []driverapi.IPAMData) (map[string]string, error) {
	return nil, types.NotImplementedErrorf("not implemented")
}

func (d *driver) NetworkFree(id string) error {
	return types.NotImplementedErrorf("not implemented")
}

func (d *driver) EventNotify(etype driverapi.EventType, nid, tableName, key string, value []byte) {
}

func (d *driver) DecodeTableEntry(tablename string, key string, value []byte) (string, map[string]string) {
	return "", nil
}

func (d *driver) CreateNetwork(id string, option map[string]interface{}, nInfo driverapi.NetworkInfo, ipV4Data, ipV6Data []driverapi.IPAMData) error {
	if len(ipV4Data) == 0 || ipV4Data[0].Pool.String() == "0.0.0.0/0" {
		return types.BadRequestErrorf("ipv4 pool is empty")
	}
	// Sanity checks
	d.Lock()
	if _, ok := d.networks[id]; ok {
		d.Unlock()
		return types.ForbiddenErrorf("network %s exists", id)
	}
	d.Unlock()

	// Parse and validate the config. It should not conflict with existing networks' config
	config, err := parseNetworkOptions(d, id, option)
	if err != nil {
		return err
	}

	err = config.processIPAM(id, ipV4Data, ipV6Data)
	if err != nil {
		return err
	}

	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()

	if err = d.createNetwork(config); err != nil {
		return err
	}

	return d.storeUpdate(config)
}

func newInterface(config *networkConfiguration) *bridgeInterface {
	var err error
	i := &bridgeInterface{}
	i.bridgeIPv4 = config.AddressIPv4
	i.gatewayIPv4 = config.AddressIPv4.IP
	if config.BridgeName == "" {
		config.BridgeName = DefaultBridgeName
	}
	i.Link, err = net.InterfaceByName(config.BridgeName)
	if err != nil {
		logrus.Debugf("Did not find any interface with name %s: %v", config.BridgeName, err)
	}
	return i
}

// exists indicates if the existing bridge interface exists on the system.
func (i *bridgeInterface) exists() bool {
	return i.Link != nil
}

// exists indicates if the existing bridge interface exists on the system.
func (i *bridgeInterface) addresses() ([]*net.IPNet, []*net.IPNet, error) {
	var addresses []*net.IPNet
	addrs, _ := i.Link.Addrs()
	for _, addr := range addrs {
		address, _ := addr.(*net.IPNet)
		if address != nil {
			addresses = append(addresses, address)
		}
	}
	return addresses, nil, nil
}


// This function prunes the pf.conf for the firewall
// that enable the service successfully.
func fixPFConf() error {
	return nil
}

func (d *driver) initFirewall() error {
	return nil
}

func (d *driver) initRouting() error {
	return nil
}

func (d *driver) configure(option map[string]interface{}) error {
	var err error

	if err = d.initFirewall(); err != nil {
		return fmt.Errorf("failed to configure firewall: %v", err)
	}
	if err = d.initRouting(); err != nil {
		return fmt.Errorf("failed to configure routing: %v", err)
	}
	if err = d.initStore(option); err != nil {
		return fmt.Errorf("failed to initialize datastore: %v", err)
	}

	return nil
}

func (d *driver) getNetwork(id string) (*bridgeNetwork, error) {
	d.Lock()
	defer d.Unlock()

	if id == "" {
		return nil, types.BadRequestErrorf("invalid network id: %s", id)
	}

	if nw, ok := d.networks[id]; ok {
		return nw, nil
	}

	return nil, types.NotFoundErrorf("network not found: %s", id)
}

// Return a slice of networks over which caller can iterate safely
func (d *driver) getNetworks() []*bridgeNetwork {
	d.Lock()
	defer d.Unlock()

	ls := make([]*bridgeNetwork, 0, len(d.networks))
	for _, nw := range d.networks {
		ls = append(ls, nw)
	}
	return ls
}

func bridgeCleanup(config *networkConfiguration, logErr bool) {
	var err error

	pfAnchor := fmt.Sprintf("docker/%s", config.ID[0:7])

	err = exec.Command("/sbin/pfctl", "-a", pfAnchor, "-F", "all").Run()
	if err != nil && logErr {
		logrus.Warn("cannot flush firewall rules")
	}
}

func (d *driver) createNetwork(config *networkConfiguration) error {
	var err error

	logrus.Infof("Creating bridge network: %s %s %s", config.ID,
		config.BridgeName, config.AddressIPv4)

	networkList := d.getNetworks()
	for i, nw := range networkList {
		nw.Lock()
		nwConfig := nw.config
		nw.Unlock()
		if err := nwConfig.Conflicts(config); err != nil {
			if config.DefaultBridge {
				// We encountered and identified a stale default network
				// We must delete it as libnetwork is the source of thruth
				// The default network being created must be the only one
				// This can happen only from docker 1.12 on ward
				logrus.Infof("Removing stale default bridge network %s (%s)", nwConfig.ID, nwConfig.BridgeName)
				if err := d.DeleteNetwork(nwConfig.ID); err != nil {
					logrus.Warnf("Failed to remove stale default network: %s (%s): %v. Will remove from store.", nwConfig.ID, nwConfig.BridgeName, err)
					d.storeDelete(nwConfig)
				}
				networkList = append(networkList[:i], networkList[i+1:]...)
			} else {
				return types.ForbiddenErrorf(
					"cannot create network %s (%s): "+
						"conflicts with network %s (%s): %s",
					nwConfig.BridgeName, config.ID, nw.id,
					nw.config.BridgeName, err.Error())
			}
		}
	}
	if config.DefaultBindingIP == nil ||
		config.DefaultBindingIP.IsUnspecified() {
		config.DefaultBindingIP = d.defrouteIP
	}

	bridgeIface := newInterface(config)
	// Create and set network handler in driver
	network := &bridgeNetwork{
		id:         config.ID,
		endpoints:  make(map[string]*bridgeEndpoint),
		config:     config,
		//portMapper: portmapper.New(""),
		driver:     d,
		bridge:     bridgeIface,
	}

	d.Lock()
	d.networks[config.ID] = network
	d.Unlock()

	// On failure make sure to reset driver network handler to nil
	defer func() {
		if err != nil {
			d.Lock()
			delete(d.networks, config.ID)
			d.Unlock()
		}
	}()

	// Verify the network configuration does not conflict with previously installed
	// networks. This step is needed now because driver might have now set the bridge
	// name on this config struct. And because we need to check for possible address
	// conflicts, so we need to check against operational networks.
	if err = config.conflictsWithNetworks(config.ID, networkList); err != nil {
		return err
	}

	// We only attempt to create the bridge when the requested device name is
	// the default one.
	if config.BridgeName != DefaultBridgeName && config.DefaultBridge {
		return NonDefaultBridgeExistError(config.BridgeName)
	}

	bridgeCleanup(config, false)
	// Prepare the bridge setup configuration
	bridgeSetup := newBridgeSetup(config, bridgeIface)

	// If the bridge interface doesn't exist, we need to start the setup steps
	// by creating a new device and assigning it an IPv4 address.
	bridgeAlreadyExists := bridgeIface.exists()
	if !bridgeAlreadyExists {
		bridgeSetup.queueStep(setupDevice)
		bridgeSetup.queueStep(setupDefaultSysctl)
	}

	// For the default bridge, set expected sysctls
	if config.DefaultBridge {
		bridgeSetup.queueStep(setupDefaultSysctl)
	}

	// Even if a bridge exists try to setup IPv4.
	bridgeSetup.queueStep(setupBridgeIPv4)

	// Apply the prepared list of steps, and abort at the first error.
	bridgeSetup.queueStep(setupDeviceUp)
	bridgeSetup.queueStep(addPFNatRule)
	return bridgeSetup.apply()
}

func (d *driver) DeleteNetwork(nid string) error {
	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()
	return d.deleteNetwork(nid)
}

func (d *driver) deleteNetwork(nid string) error {
	var err error

	// Get network handler and remove it from driver
	d.Lock()
	n, ok := d.networks[nid]
	d.Unlock()

	if !ok {
		return types.InternalMaskableErrorf("network %s does not exist", nid)
	}
	n.Lock()
	config := n.config
	n.Unlock()

	for _, ep := range n.endpoints {
		if err := n.releasePorts(ep); err != nil {
			logrus.Warn(err)
		}

		err = exec.Command("/sbin/ifconfig", ep.srcName, "destroy").Run()
		if err != nil {
			logrus.Warn("Error destroying %s: %+v\n", ep.srcName, err)
		}
                if err := d.storeDelete(ep); err != nil {
                        logrus.Warnf("Failed to remove bridge endpoint %.7s from store: %v", ep.id, err)
                }
	}

        d.Lock()
        delete(d.networks, nid)
        d.Unlock()

	// On failure set network handler back in driver, but
	// only if is not already taken over by some other thread
	defer func() {
		if err != nil {
			d.Lock()
			if _, ok := d.networks[nid]; !ok {
				d.networks[nid] = n
			}
			d.Unlock()
		}
	}()

        switch config.BridgeIfaceCreator {
        case ifaceCreatedByLibnetwork, ifaceCreatorUnknown:
                // We only delete the bridge if it was created by the bridge driver and
                // it is not the default one (to keep the backward compatible behavior.)
                if !config.DefaultBridge {
                        if err := exec.Command("/sbin/ifconfig", config.BridgeName, "destroy").Run(); err != nil {
                                logrus.Warnf("Failed to remove bridge interface %s on network %s delete: %v", config.BridgeName, nid, err)
                        }
                }
        case ifaceCreatedByUser:
                // Don't delete the bridge interface if it was not created by libnetwork.
        }

	// Sanity check
	if n == nil {
		err = driverapi.ErrNoNetwork(nid)
		return err
	}

	// Cannot remove network if endpoints are still present
	if len(n.endpoints) != 0 {
		err = ActiveEndpointsError(n.id)
		return err
	}
	bridgeCleanup(n.config, true)
	logrus.Infof("Deleting bridge network: %s", nid[:12])
	return d.storeDelete(n.config)
}

func (d *driver) CreateEndpoint(nid, eid string, ifInfo driverapi.InterfaceInfo, epOptions map[string]interface{}) error {
	if ifInfo == nil {
		return errors.New("invalid interface passed")
	}

	// Get the network handler and make sure it exists
	d.Lock()
	n, ok := d.networks[nid]
	d.Unlock()

	if !ok {
		return types.NotFoundErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return driverapi.ErrNoNetwork(nid)
	}

	// Sanity check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return InvalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check if endpoint id is good and retrieve correspondent endpoint
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return err
	}

	// Endpoint with that id exists either on desired or other sandbox
	if ep != nil {
		return driverapi.ErrEndpointExists(eid)
	}
	// Try to convert the options to endpoint configuration
	epConfig, err := parseEndpointOptions(epOptions)
	if err != nil {
		return err
	}

	// Create and add the endpoint
	n.Lock()
	endpoint := &bridgeEndpoint{id: eid, config: epConfig}
	n.endpoints[eid] = endpoint
	n.Unlock()

	// On failure make sure to remove the endpoint
	defer func() {
		if err != nil {
			n.Lock()
			delete(n.endpoints, eid)
			n.Unlock()
		}
	}()
        // Generate a name for what will be the host side pipe interface
        hostIfName, err := netutils.GenerateIfaceName(vethPrefix, vethLen)
        if err != nil {
                return err
        }

        // Generate a name for what will be the sandbox side pipe interface
	containerIfName, err := netutils.GenerateIfaceNameFromEID(vethPrefix, vethLen, eid)
        if err != nil {
                return err
        }
	//create epair interface
	out, err := exec.Command("/sbin/ifconfig", "epair", "create", "group", "docker").Output()
	if err != nil {
		return err
	}
	epaira := strings.TrimSuffix(string(out), "\n")

	//rename host interface
	err = exec.Command("/sbin/ifconfig", epaira, "name", hostIfName, "up").Run()
	if err != nil {
		return err
	}

	bif := strings.TrimSuffix(epaira, "a") + "b"

	//rename containerinterface
	err = exec.Command("/sbin/ifconfig", bif, "name", containerIfName).Run()
	if err != nil {
		return err
	}
	// Create the sandbox side pipe interface
	if ifInfo.MacAddress() == nil {
		// No MAC address assigned to interface. Generate a random MAC to assign
		endpoint.macAddress = netutils.GenerateRandomMAC()
		if err := ifInfo.SetMacAddress(endpoint.macAddress); err != nil {
			logrus.Warnf("Unable to set mac address: %s to endpoint: %s",
				endpoint.macAddress.String(), endpoint.id)
			return err
		}
	} else {
		endpoint.macAddress = ifInfo.MacAddress()
	}
	endpoint.srcName = containerIfName
	endpoint.addr = ifInfo.Address()
	endpoint.addrv6 = ifInfo.AddressIPv6()
	n.Lock()
	c := n.config
	n.Unlock()
	err = exec.Command("/sbin/ifconfig", c.BridgeName, "addm", hostIfName).Run()
	if err != nil {
		return err
	}
	// Program any required port mapping and store them in the endpoint
	endpoint.portMapping, err = n.allocatePorts(endpoint, c.DefaultBindingIntf, c.DefaultBindingIP, true)
	if err != nil {
		return err
	}

        if err = d.storeUpdate(endpoint); err != nil {
                return fmt.Errorf("failed to save bridge endpoint %.7s to store: %v", endpoint.id, err)
        }

	return nil
}

func (d *driver) removeAndDestroyEndpoint(ifname string, containerID string) error {
		var err error
                err = exec.Command("/sbin/ifconfig", ifname, "destroy").Run()
                if err != nil {
                        logrus.Warn("Error destroying %s: %+v\n", ifname, err)
                }
		return nil
}

func (d *driver) DeleteEndpoint(nid, eid string) error {
	var err error

	// Get the network handler and make sure it exists
	d.Lock()
	n, ok := d.networks[nid]
	d.Unlock()

	if !ok {
		return types.InternalMaskableErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return driverapi.ErrNoNetwork(nid)
	}

	// Sanity Check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return InvalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check endpoint id and if an endpoint is actually there
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return err
	}
	if ep == nil {
		return EndpointNotFoundError(eid)
	}

	// Remove it
	n.Lock()
	delete(n.endpoints, eid)
	n.Unlock()

	// On failure make sure to set back ep in n.endpoints, but only
	// if it hasn't been taken over already by some other thread.
	defer func() {
		if err != nil {
			n.Lock()
			if _, ok := n.endpoints[eid]; !ok {
				n.endpoints[eid] = ep
			}
			n.Unlock()
		}
	}()

	d.removeAndDestroyEndpoint(ep.srcName, ep.config.containerID)

        if err := d.storeDelete(ep); err != nil {
                logrus.Warnf("Failed to remove bridge endpoint %.7s from store: %v", ep.id, err)
        }

	return nil
}

func (d *driver) EndpointOperInfo(nid, eid string) (map[string]interface{}, error) {
	// Get the network handler and make sure it exists
	d.Lock()
	n, ok := d.networks[nid]
	d.Unlock()
	if !ok {
		return nil, types.NotFoundErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return nil, driverapi.ErrNoNetwork(nid)
	}

	// Sanity check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return nil, InvalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check if endpoint id is good and retrieve correspondent endpoint
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return nil, err
	}
	if ep == nil {
		return nil, driverapi.ErrNoEndpoint(eid)
	}

	m := make(map[string]interface{})

	if ep.extConnConfig != nil && ep.extConnConfig.ExposedPorts != nil {
		// Return a copy of the config data
		epc := make([]types.TransportPort, 0, len(ep.extConnConfig.ExposedPorts))
		for _, tp := range ep.extConnConfig.ExposedPorts {
			epc = append(epc, tp.GetCopy())
		}
		m[netlabel.ExposedPorts] = epc
	}

	if ep.portMapping != nil {
		// Return a copy of the operational data
		pmc := make([]types.PortBinding, 0, len(ep.portMapping))
		for _, pm := range ep.portMapping {
			pmc = append(pmc, pm.GetCopy())
		}
		m[netlabel.PortMap] = pmc
	}

	if len(ep.macAddress) != 0 {
		m[netlabel.MacAddress] = ep.macAddress
	}
	return m, nil
}

// Join method is invoked when a Sandbox is attached to an endpoint.
func (d *driver) Join(nid, eid string, sboxKey string, jinfo driverapi.JoinInfo, options map[string]interface{}) error {
        network, err := d.getNetwork(nid)
        if err != nil {
                return err
        }

        endpoint, err := network.getEndpoint(eid)
        if err != nil {
                return err
        }

        if endpoint == nil {
                return EndpointNotFoundError(eid)
        }

        endpoint.containerConfig, err = parseContainerOptions(options)
        if err != nil {
                return err
        }

        iNames := jinfo.InterfaceName()
        containerVethPrefix := defaultContainerVethPrefix
        if network.config.ContainerIfacePrefix != "" {
                containerVethPrefix = network.config.ContainerIfacePrefix
        }
        err = iNames.SetNames(endpoint.srcName, containerVethPrefix)
        if err != nil {
                return err
        }

        err = jinfo.SetGateway(network.bridge.gatewayIPv4)
        if err != nil {
                return err
        }

        err = jinfo.SetGatewayIPv6(network.bridge.gatewayIPv6)
        if err != nil {
                return err
        }

        return nil
}

func (d *driver) link(network *bridgeNetwork, endpoint *bridgeEndpoint, enable bool) error {
	return nil
}

// Leave method is invoked when a Sandbox detaches from an endpoint.
func (d *driver) Leave(nid, eid string) error {
        network, err := d.getNetwork(nid)
        if err != nil {
                return types.InternalMaskableErrorf("%s", err)
        }

        endpoint, err := network.getEndpoint(eid)
        if err != nil {
                return err
        }

        if endpoint == nil {
                return EndpointNotFoundError(eid)
        }

        if !network.config.EnableICC {
                if err = d.link(network, endpoint, false); err != nil {
                        return err
                }
        }

        return nil
}

func (d *driver) ProgramExternalConnectivity(nid, eid string, options map[string]interface{}) error {
	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	if endpoint == nil {
		return EndpointNotFoundError(eid)
	}

	endpoint.extConnConfig, err = parseConnectivityOptions(options)
	if err != nil {
		return err
	}

	// Program any required port mapping and store them in the endpoint
	endpoint.portMapping, err = network.allocatePorts(endpoint, network.config.DefaultBindingIntf, network.config.DefaultBindingIP, true)
	if err != nil {
		return err
	}

	if !network.config.EnableICC {
		return d.link(network, endpoint, true)
	}

	return nil
}

func (d *driver) RevokeExternalConnectivity(nid, eid string) error {
	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	if endpoint == nil {
		return EndpointNotFoundError(eid)
	}

	err = network.releasePorts(endpoint)
	if err != nil {
		logrus.Warn(err)
	}

	return nil
}

func (d *driver) Type() string {
	return networkType
}

func (d *driver) IsBuiltIn() bool {
	return true
}

// DiscoverNew is a notification for a new discovery event, such as a new node joining a cluster
func (d *driver) DiscoverNew(dType discoverapi.DiscoveryType, data interface{}) error {
	return nil
}

// DiscoverDelete is a notification for a discovery delete event, such as a node leaving a cluster
func (d *driver) DiscoverDelete(dType discoverapi.DiscoveryType, data interface{}) error {
	return nil
}

// Validate performs a static validation on the network configuration parameters.
// Whatever can be assessed a priori before attempting any programming.
func (c *networkConfiguration) Validate() error {
	if c.Mtu < 0 {
		return ErrInvalidMtu(c.Mtu)
	}

	// If bridge v4 subnet is specified
	if c.AddressIPv4 != nil {
		// If default gw is specified, it must be part of bridge subnet
		if c.DefaultGatewayIPv4 != nil {
			if !c.AddressIPv4.Contains(c.DefaultGatewayIPv4) {
				return &ErrInvalidGateway{}
			}
		}
	}

	// If default v6 gw is specified, AddressIPv6 must be specified and gw must belong to AddressIPv6 subnet
	if c.EnableIPv6 && c.DefaultGatewayIPv6 != nil {
		if c.AddressIPv6 == nil || !c.AddressIPv6.Contains(c.DefaultGatewayIPv6) {
			return &ErrInvalidGateway{}
		}
	}
	return nil
}

// Checks whether this network's configuration for the network with this id conflicts with any of the passed networks
func (c *networkConfiguration) conflictsWithNetworks(id string, others []*bridgeNetwork) error {
	for _, nw := range others {

		nw.Lock()
		nwID := nw.id
		nwConfig := nw.config
		nwBridge := nw.bridge
		nw.Unlock()

		if nwID == id {
			continue
		}
		// Verify the name (which may have been set by newInterface()) does not conflict with
		// existing bridge interfaces. Ironically the system chosen name gets stored in the config...
		// Basically we are checking if the two original configs were both empty.
		if nwConfig.BridgeName == c.BridgeName {
			return types.ForbiddenErrorf("conflicts with network %s (%s) by bridge name", nwID, nwConfig.BridgeName)
		}
		// If this network config specifies the AddressIPv4, we need
		// to make sure it does not conflict with any previously allocated
		// bridges. This could not be completely caught by the config conflict
		// check, because networks which config does not specify the AddressIPv4
		// get their address and subnet selected by the driver (see electBridgeIPv4())
		if c.AddressIPv4 != nil {
			if nwBridge.bridgeIPv4.Contains(c.AddressIPv4.IP) ||
				c.AddressIPv4.Contains(nwBridge.bridgeIPv4.IP) {
				return types.ForbiddenErrorf("conflicts with network %s (%s) by ip network", nwID, nwConfig.BridgeName)
			}
		}
	}

	return nil
}

// Conflicts check if two NetworkConfiguration objects overlap
func (c *networkConfiguration) Conflicts(o *networkConfiguration) error {
	if o == nil {
		return fmt.Errorf("same configuration")
	}

	// Also empty, because only one network with empty name is allowed
	if c.BridgeName == o.BridgeName {
		return fmt.Errorf("networks have same bridge name")
	}

	// They must be in different subnets
	if (c.AddressIPv4 != nil && o.AddressIPv4 != nil) &&
		(c.AddressIPv4.Contains(o.AddressIPv4.IP) || o.AddressIPv4.Contains(c.AddressIPv4.IP)) {
		return fmt.Errorf("networks have overlapping IPv4")
	}

	// They must be in different v6 subnets
	if (c.AddressIPv6 != nil && o.AddressIPv6 != nil) &&
		(c.AddressIPv6.Contains(o.AddressIPv6.IP) || o.AddressIPv6.Contains(c.AddressIPv6.IP)) {
		return fmt.Errorf("networks have overlapping IPv6")
	}

	return nil
}

func (c *networkConfiguration) fromLabels(labels map[string]string) error {
	var err error
	for label, value := range labels {
		switch label {
		case BridgeName:
			c.BridgeName = value
		case netlabel.DriverMTU:
			if c.Mtu, err = strconv.Atoi(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case netlabel.EnableIPv6:
			if c.EnableIPv6, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case EnableIPMasquerade:
			if c.EnableIPMasquerade, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case EnableICC:
			if c.EnableICC, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case DefaultBridge:
			if c.DefaultBridge, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case DefaultBindingIP:
			if c.DefaultBindingIP = net.ParseIP(value); c.DefaultBindingIP == nil {
				return parseErr(label, value, "nil ip")
			}
		}
	}

	return nil
}

func parseErr(label, value, errString string) error {
	return types.BadRequestErrorf("failed to parse %s value: %v (%s)", label, value, errString)
}

func parseNetworkGenericOptions(data interface{}) (*networkConfiguration, error) {
	var (
		err    error
		config *networkConfiguration
	)

	switch opt := data.(type) {
	case *networkConfiguration:
		config = opt
	case map[string]string:
		config = &networkConfiguration{
			EnableICC:          true,
			EnableIPMasquerade: true,
		}
		err = config.fromLabels(opt)
	case options.Generic:
		var opaqueConfig interface{}
		if opaqueConfig, err = options.GenerateFromModel(opt, config); err == nil {
			config = opaqueConfig.(*networkConfiguration)
		}
	default:
		err = types.BadRequestErrorf("do not recognize network configuration format: %T", opt)
	}

	return config, err
}

func parseNetworkOptions(d *driver, id string, option options.Generic) (*networkConfiguration, error) {
	var (
		err    error
		config = &networkConfiguration{}
	)

	// Parse generic label first, config will be re-assigned
	if genData, ok := option[netlabel.GenericData]; ok && genData != nil {
		if config, err = parseNetworkGenericOptions(genData); err != nil {
			return nil, err
		}
	}

	// Process well-known labels next
	if val, ok := option[netlabel.EnableIPv6]; ok {
		config.EnableIPv6 = val.(bool)
	}

	if val, ok := option[netlabel.Internal]; ok {
		if internal, ok := val.(bool); ok && internal {
			config.Internal = true
		}
	}

	// Finally validate the configuration
	if err = config.Validate(); err != nil {
		return nil, err
	}

	if config.BridgeName == "" && config.DefaultBridge == false {
		config.BridgeName = "br_" + id[:12] + "_0"
	}

	lastChar := config.BridgeName[len(config.BridgeName)-1:]
	if _, err = strconv.Atoi(lastChar); err != nil {
		config.BridgeNameInternal = config.BridgeName + "_0"
	} else {
		config.BridgeNameInternal = config.BridgeName
	}

	exists, err := bridgeInterfaceExists(config.BridgeName)
	if err != nil {
		return nil, err
	}

	if !exists {
		config.BridgeIfaceCreator = ifaceCreatedByLibnetwork
	} else {
		config.BridgeIfaceCreator = ifaceCreatedByUser
	}

	config.ID = id
	return config, nil
}

func bridgeInterfaceExists(name string) (bool, error) {
	_, err := net.InterfaceByName(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check bridge interface esistence: %v", err)
	}
	return true, nil
}

func (c *networkConfiguration) processIPAM(id string, ipamV4Data, ipamV6Data []driverapi.IPAMData) error {
	if len(ipamV4Data) > 1 || len(ipamV6Data) > 1 {
		return types.ForbiddenErrorf("bridge driver doesnt support multiple subnets")
	}

	if len(ipamV4Data) == 0 {
		return types.BadRequestErrorf("bridge network %s requires ipv4 configuration", id)
	}

	if ipamV4Data[0].Gateway != nil {
		c.AddressIPv4 = types.GetIPNetCopy(ipamV4Data[0].Gateway)
	}

	if gw, ok := ipamV4Data[0].AuxAddresses[DefaultGatewayV4AuxKey]; ok {
		c.DefaultGatewayIPv4 = gw.IP
	}

	if len(ipamV6Data) > 0 {
		c.AddressIPv6 = ipamV6Data[0].Pool

		if ipamV6Data[0].Gateway != nil {
			c.AddressIPv6 = types.GetIPNetCopy(ipamV6Data[0].Gateway)
		}

		if gw, ok := ipamV6Data[0].AuxAddresses[DefaultGatewayV6AuxKey]; ok {
			c.DefaultGatewayIPv6 = gw.IP
		}
	}

	return nil
}

func (n *bridgeNetwork) getEndpoint(eid string) (*bridgeEndpoint, error) {
	n.Lock()
	defer n.Unlock()

	if eid == "" {
		return nil, InvalidEndpointIDError(eid)
	}

	if ep, ok := n.endpoints[eid]; ok {
		return ep, nil
	}

	return nil, nil
}

func parseEndpointOptions(epOptions map[string]interface{}) (*endpointConfiguration, error) {
	if epOptions == nil {
		return nil, nil
	}

	ec := &endpointConfiguration{}

	if opt, ok := epOptions[netlabel.MacAddress]; ok {
		if mac, ok := opt.(net.HardwareAddr); ok {
			ec.MacAddress = mac
		} else {
			return nil, &ErrInvalidEndpointConfig{}
		}
	}

	if opt, ok := epOptions[netlabel.PortMap]; ok {
		if bs, ok := opt.([]types.PortBinding); ok {
			ec.PortBindings = bs
		} else {
			return nil, &ErrInvalidEndpointConfig{}
		}
	}

	if opt, ok := epOptions[netlabel.ExposedPorts]; ok {
		if ports, ok := opt.([]types.TransportPort); ok {
			ec.ExposedPorts = ports
		} else {
			return nil, &ErrInvalidEndpointConfig{}
		}
	}

	if opt, ok := epOptions["containerid"].(string); ok {
		ec.containerID = opt
	} else {
		return nil, &ErrInvalidEndpointConfig{}
	}

	return ec, nil
}

func parseContainerOptions(cOptions map[string]interface{}) (*containerConfiguration, error) {
	if cOptions == nil {
		return nil, nil
	}
	genericData := cOptions[netlabel.GenericData]
	if genericData == nil {
		return nil, nil
	}
	switch opt := genericData.(type) {
	case options.Generic:
		opaqueConfig, err := options.GenerateFromModel(opt, &containerConfiguration{})
		if err != nil {
			return nil, err
		}
		return opaqueConfig.(*containerConfiguration), nil
	case *containerConfiguration:
		return opt, nil
	default:
		return nil, nil
	}
}

func parseConnectivityOptions(cOptions map[string]interface{}) (*connectivityConfiguration, error) {
	if cOptions == nil {
		return nil, nil
	}

	cc := &connectivityConfiguration{}

	if opt, ok := cOptions[netlabel.PortMap]; ok {
		if pb, ok := opt.([]types.PortBinding); ok {
			cc.PortBindings = pb
		} else {
			return nil, types.BadRequestErrorf("Invalid port mapping data in connectivity configuration: %v", opt)
		}
	}

	if opt, ok := cOptions[netlabel.ExposedPorts]; ok {
		if ports, ok := opt.([]types.TransportPort); ok {
			cc.ExposedPorts = ports
		} else {
			return nil, types.BadRequestErrorf("Invalid exposed ports data in connectivity configuration: %v", opt)
		}
	}

	return cc, nil
}
