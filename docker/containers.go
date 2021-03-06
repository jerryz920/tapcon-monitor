package docker

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	docker_type "github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/container"
	config "github.com/jerryz920/tapcon-monitor/config"
	metadata "github.com/jerryz920/tapcon-monitor/statement"
)

const (
	CONTAINER_CONFIG_FILE = "config.v2.json"
	CONTAINER_HOST_CONFIG = "hostconfig.json"
	LOCALHOST_V4          = "127.0.0.1"
	DEFAULT_NS            = "default"
	WILDCARD_IP           = "0.0.0.0"

	/// Container events
	NEED_UPDATE    = 1
	CONTAINER_DEAD = 2
)

type instanceIp struct {
	ns string
	ip string
}

type MemContainer struct {
	Config           *docker.Container
	Id               string
	Root             string // /var/lib/docker/containers/<id>/
	Mutex            *sync.Mutex
	Ips              []string /// detect the IPv4 addresses of this container
	PrincipalCreated bool
	FactCreated      bool
	ImageLinked      bool
	PortAliasCreated bool // only applies to non-static ports
	StaticPortMin    int
	StaticPortMax    int
	LocalNs          string
	LastUpdate       time.Time
	LastRefresh      time.Time
	Cache            ReconcileCache
	RefreshDuration  time.Duration
	VmIps            []instanceIp
	EventChan        chan int
	listIp           func(string) []string
}

func NewMemContainer(id, root, localNs string) *MemContainer {
	return &MemContainer{
		Config:          nil,
		Id:              id,
		Mutex:           &sync.Mutex{},
		Root:            root,
		listIp:          ListNsIps,
		EventChan:       make(chan int),
		LocalNs:         localNs,
		Cache:           nil,
		RefreshDuration: config.Config.Daemon.RefreshTimeout * time.Second,
	}
}

func (c *MemContainer) OutOfDate() bool {
	if c.Config == nil {
		return true
	}

	configFile := path.Join(c.Root, CONTAINER_CONFIG_FILE)
	result, err := os.Stat(configFile)
	if err != nil {
		/// the container might not yet have config, skip
		c.Config = nil
		return false
	}

	if c.LastUpdate.Before(result.ModTime()) {
		log.Debugf("%s config is newer", c.Id)
		return true
	}

	hostConfig := path.Join(c.Root, CONTAINER_HOST_CONFIG)
	result, err = os.Stat(hostConfig)
	if err != nil {
		// same as config file, but set Config as nil
		c.Config = nil
		return false
	}

	if c.LastUpdate.Before(result.ModTime()) {
		log.Debugf("%s host config is newer", c.Id)
		return true
	}
	return false
}

func (c *MemContainer) recordTimestamp() {
	/// after a successful update, the timestamp should be recorded
	configFile := path.Join(c.Root, CONTAINER_CONFIG_FILE)
	result, err := os.Stat(configFile)
	if err != nil {
		/// config and hostconfig gone, revert the loaded content
		log.Debugf("config file %s gone during loading", configFile)
		c.Config = nil
		return
	}

	c.LastUpdate = result.ModTime()

	hostConfig := path.Join(c.Root, CONTAINER_HOST_CONFIG)
	result, err = os.Stat(hostConfig)
	if err != nil {
		// same as config file, but set Config as nil
		log.Debugf("host config file %s gone during loading", hostConfig)
		c.Config = nil
		return
	}

	if c.LastUpdate.Before(result.ModTime()) {
		c.LastUpdate = result.ModTime()
	}
}

func (c *MemContainer) ResetState() {
	c.PrincipalCreated = false
	c.PortAliasCreated = false
	c.ImageLinked = false
	c.FactCreated = false
}

// Load indicates whether the container contains a valid running config file
func (c *MemContainer) Load() bool {
	//c.Mutex.Lock()
	//defer c.Mutex.Unlock()
	if c.OutOfDate() {
		// do load, a race condition can happen if we record timestamp later:
		// when host config is updated we checked it should be loaded, then
		// record timestamp. But if some update happens between loading and
		// record timestamp, there will be missing of update, because the
		// latest timestamp is recorded, whereas the older content is loaded
		log.Debugf("loading container %s", c.Id)
		oldTimestamp := c.LastUpdate
		c.recordTimestamp()
		baseContainer := docker.NewBaseContainer(c.Id, c.Root)
		if err := baseContainer.FromDisk(); err != nil {
			log.Errorf("loading the container content: %v", err)
			/// Revert to old timestamp so next time we try to update again
			c.LastUpdate = oldTimestamp
			return false
		}
		c.Config = baseContainer
		log.Debugf("container status: %v", baseContainer.Running)

		if !baseContainer.Running {
			//log.Printf("stopped container %s\n", c.Id)
			return false
		}
		osNsName := baseContainer.NetworkSettings.SandboxKey
		if osNsName == "" {
			/// for this case the container is still loaded, but just not running
			c.Ips = make([]string, 0)
			return true
		}
		log.Debugf("checking sandbox key: %s", osNsName)
		ips := c.listIp(osNsName)
		if len(ips) == 0 && !baseContainer.Config.NetworkDisabled {
			log.Errorf("There must be non-empty ip list for container")
			/// for this case the container is still loaded, but just not running
			c.Ips = make([]string, 0)
			return true
		}
		log.Debugf("listing Ips: %s", ips)
		c.Ips = ips
		return true
	}
	if c.Config == nil {
		return false
	}
	return true
}

func (c *MemContainer) AssignStaticPorts(pmin, pmax int) {
	c.StaticPortMin = pmin
	c.StaticPortMax = pmax
}

func (c *MemContainer) Running() bool {
	// We don't need to lock it as it's read only
	if c.Config == nil {
		return false
	}
	return c.Config.State.Running
}

func IsBridgeNetwork(name string) bool {
	mode := docker_type.NetworkMode(name)
	return mode.IsBridge()
}
func IsUserOverlayNetwork(name string) bool {
	mode := docker_type.NetworkMode(name)
	return mode.IsUserDefined()
}

func (c *MemContainer) GetNsName(ip string) (string, error) {

	for name, network := range c.Config.NetworkSettings.Networks {
		if network.IPAddress == ip {
			if IsUserOverlayNetwork(name) {
				return network.NetworkID, nil
			} else if IsBridgeNetwork(name) {
				return c.LocalNs, nil
			}
		}
	}
	if c.Config.HostConfig.NetworkMode.IsUserDefined() {
		// Then the user defined network, the network is "hidden" but it
		// is there, and it will be the IP we are looking for.
		for _, nip := range c.Ips {
			if ip == nip {
				return c.LocalNs, nil
			}
		}
	}

	//	In this case there might be "hidden" networks used for the
	//	container. However, that IP address is not overlayed, nor
	//  an instance IP, so we dont have ns name for it, and we wont
	//  need to run attestation on this IP address
	return "", fmt.Errorf("Can't find NS name for IP %s", ip)
}

/// This function needs more elaboration: bridge, gw_bridge, and many other things
func IsConnectedToHostNetwork(name string) bool {
	return name == "bridge"
}

func (c *MemContainer) IsContainerIp(ip string) bool {
	for name, network := range c.Config.NetworkSettings.Networks {
		if network.IPAddress == ip && IsBridgeNetwork(name) {
			return true
		}
	}
	if c.Config.HostConfig.NetworkMode.IsUserDefined() {
		// Then the user defined network, the network is "hidden" but it
		// is there, and it will be the IP we are looking for.
		for _, nip := range c.Ips {
			if ip == nip {
				return true
			}
		}
	}
	return false
}

func (c *MemContainer) Dump() {
	log.Infof("Id: %s", c.Id)
	if c.Config == nil {
		log.Infof("nil state")
		return
	}
	log.Infof("State: %v", c.Config.Running)
	log.Infof("Root: %s", c.Root)
	log.Infof("Ips: %v", c.Ips)
	log.Infof("StaticPorts: %d %d", c.StaticPortMin, c.StaticPortMax)
	log.Infof("")
}

func (c *MemContainer) ContainerFacts() []metadata.Statement {
	if c.Config == nil {
		return []metadata.Statement{}
	}
	cid := tapconContainerId(c)
	iid := tapconContainerImageId(c)
	return []metadata.Statement{
		metadata.Statement(fmt.Sprintf("containerFact(\"%s\", \"%s\")", cid, iid))}
}

/// Ports for public network usage
func (c *MemContainer) ContainerPorts() []PortAlias {
	ports := make([]PortAlias, 0, len(c.Config.NetworkSettings.Ports)+1)
	/// Fixme: here we should actually return the port binding for the host
	for _, bindings := range c.Config.NetworkSettings.Ports {
		for _, binding := range bindings {
			p64, err := strconv.ParseInt(binding.HostPort, 10, 0)
			if err != nil {
				log.Errorf("parsing port alias %v", binding.HostPort)
				continue
			}
			p := int(p64)
			for _, proto := range [2]string{"tcp", "udp"} {
				for _, vmip := range c.VmIps {
					ports = append(ports, PortAlias{
						min:      p,
						max:      p,
						ip:       vmip.ip,
						protocol: proto,
						nsName:   vmip.ns,
					})
				}
			}
		}
	}
	if c.StaticPortMin != 0 {
		for _, proto := range [2]string{"tcp", "udp"} {
			for _, vmip := range c.VmIps {
				ports = append(ports, PortAlias{
					min:      c.StaticPortMin,
					max:      c.StaticPortMax,
					ip:       vmip.ip,
					protocol: proto,
					nsName:   vmip.ns,
				})
			}
		}
	}
	return ports
}

func (c *MemContainer) Refresh() error {
	now := time.Now()
	if now.After(c.LastRefresh.Add(c.RefreshDuration)) {
		if err := c.Cache.Refresh(); err != nil {
			if c.Cache.Valid() {
				log.Errorf("refreshing valid cache: %v", err)
			}
			return err
		} else {
			c.LastRefresh = now
		}
	}
	return nil
}

//func modifiedSinceLast(last_update time.Time, root string) (bool, error) {
//	config_file := path.Join(root, CONTAINER_CONFIG_FILE)
//	if result, err := os.Stat(config_file); err != nil {
//		return false, err
//	} else {
//		return last_update.Before(result.ModTime()), nil
//	}
//}
//
//func LoadContainer(id string, root string, last_update time.Time,
//	force bool) (*docker.Container, error) {
//	if !force {
//		if ok, err := modifiedSinceLast(last_update, root); err != nil || !ok {
//			return nil, err
//		}
//	}
//	baseContainer := docker.NewBaseContainer(id, root)
//	if err := baseContainer.FromDisk(); err != nil {
//		log.Print("error in loading the container content: ", err)
//		return nil, err
//	}
//	return baseContainer, nil
//
//}
//
//func LoadMemContainer(id string, root string) (*MemContainer, error) {
//	baseContainer, err := LoadContainer(id, root, time.Now(), true)
//	if err != nil {
//		return nil, err
//	}
//	return &MemContainer{Config: baseContainer, Id: id, Root: root}, nil
//}
//
//func ContainerInspect(c *docker.Container) {
//	log.Printf("----------------------")
//	log.Printf("container id: %s, running: %v\n", c.ID, c.Running)
//	log.Printf("container start at: %s, finish at: %s\n", c.StartedAt, c.FinishedAt)
//	log.Printf("container ImageID: %v\n", c.ImageID)
//	log.Printf("container config Image: %s", c.Config.Image)
//}
//
func ContainerConfigPaths(root string) []string {
	return []string{path.Join(root, CONTAINER_CONFIG_FILE),
		path.Join(root, CONTAINER_HOST_CONFIG)}
}

func IsContainerPath(p, root string) bool {
	dirname := filepath.Dir(p)
	return dirname == root
}

func ContainerPathToId(p, root string) string {
	if IsContainerPath(p, root) {
		return filepath.Base(p)
	} else {
		cp := filepath.Dir(p)
		return filepath.Base(cp)
	}
}

func ContainerConfigFile(p string) bool {
	return p == CONTAINER_CONFIG_FILE || p == CONTAINER_HOST_CONFIG
}
