package daemon

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/container"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/stringid"
	volumestore "github.com/docker/docker/volume/store"
	"github.com/docker/engine-api/types"
	containertypes "github.com/docker/engine-api/types/container"
	networktypes "github.com/docker/engine-api/types/network"
	"github.com/opencontainers/runc/libcontainer/label"
)

// ContainerCreate creates a container.
func (daemon *Daemon) ContainerCreate(params types.ContainerCreateConfig) (types.ContainerCreateResponse, error) {
	fmt.Println("daemon...ContainerCreate")

	if params.Config == nil {
		return types.ContainerCreateResponse{}, fmt.Errorf("Config cannot be empty in order to create a container")
	}

	warnings, err := daemon.verifyContainerSettings(params.HostConfig, params.Config, false)
	if err != nil {
		return types.ContainerCreateResponse{Warnings: warnings}, err
	}

	err = daemon.verifyNetworkingConfig(params.NetworkingConfig)
	if err != nil {
		return types.ContainerCreateResponse{}, err
	}

	if params.HostConfig == nil {
		params.HostConfig = &containertypes.HostConfig{}
	}
	err = daemon.adaptContainerSettings(params.HostConfig, params.AdjustCPUShares)
	if err != nil {
		return types.ContainerCreateResponse{Warnings: warnings}, err
	}

	container, err := daemon.create(params)
	if err != nil {
		return types.ContainerCreateResponse{Warnings: warnings}, daemon.imageNotExistToErrcode(err)
	}

	return types.ContainerCreateResponse{ID: container.ID, Warnings: warnings}, nil
}

// Create creates a new container from the given configuration with a given name.
func (daemon *Daemon) create(params types.ContainerCreateConfig) (retC *container.Container, retErr error) {
	var (
		container *container.Container
		img       *image.Image
		imgID     image.ID
		err       error
		layersP   string
	)

	if params.Config.Image != "" {
		fmt.Println("daemon/create.go...Config.Image...", params.Config.Image)
		tmp := strings.Split(params.Config.Image, "_")
		fmt.Println(tmp[1:])
		layersP, err = compose(tmp[1:])
		if err != nil {
			fmt.Println("compose error")
			return nil, err
		}

		img, err = daemon.GetImage(tmp[0])
		if err != nil {
			return nil, err
		}
		imgID = img.ID()
	}

	if err := daemon.mergeAndVerifyConfig(params.Config, img); err != nil {
		return nil, err
	}

	if container, err = daemon.newContainer(params.Name, params.Config, imgID); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := daemon.ContainerRm(container.ID, &types.ContainerRmConfig{ForceRemove: true}); err != nil {
				logrus.Errorf("Clean up Error! Cannot destroy container %s: %v", container.ID, err)
			}
		}
	}()

	if err := daemon.setSecurityOptions(container, params.HostConfig); err != nil {
		return nil, err
	}

	// Set RWLayer for container after mount labels have been set
	if err := daemon.setRWLayer(container); err != nil {
		return nil, err
	}

	if err := daemon.Register(container); err != nil {
		return nil, err
	}
	rootUID, rootGID, err := idtools.GetRootUIDGID(daemon.uidMaps, daemon.gidMaps)
	if err != nil {
		return nil, err
	}
	if err := idtools.MkdirAs(container.Root, 0700, rootUID, rootGID); err != nil {
		return nil, err
	}

	if err := daemon.setHostConfig(container, params.HostConfig); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := daemon.removeMountPoints(container, true); err != nil {
				logrus.Error(err)
			}
		}
	}()

	if err := daemon.createContainerPlatformSpecificSettings(container, params.Config, params.HostConfig); err != nil {
		return nil, err
	}

	var endpointsConfigs map[string]*networktypes.EndpointSettings
	if params.NetworkingConfig != nil {
		endpointsConfigs = params.NetworkingConfig.EndpointsConfig
	}

	if err := daemon.updateContainerNetworkSettings(container, endpointsConfigs); err != nil {
		return nil, err
	}

	if err := container.ToDiskLocking(); err != nil {
		logrus.Errorf("Error saving new container to disk: %v", err)
		return nil, err
	}
	daemon.LogContainerEvent(container, "create")

	if err := restore(layersP); err != nil {
		fmt.Println("restore error")
		return nil, err
	}

	return container, nil
}

func (daemon *Daemon) generateSecurityOpt(ipcMode containertypes.IpcMode, pidMode containertypes.PidMode) ([]string, error) {
	if ipcMode.IsHost() || pidMode.IsHost() {
		return label.DisableSecOpt(), nil
	}
	if ipcContainer := ipcMode.Container(); ipcContainer != "" {
		c, err := daemon.GetContainer(ipcContainer)
		if err != nil {
			return nil, err
		}

		return label.DupSecOpt(c.ProcessLabel), nil
	}
	return nil, nil
}

func (daemon *Daemon) setRWLayer(container *container.Container) error {
	var layerID layer.ChainID
	if container.ImageID != "" {
		img, err := daemon.imageStore.Get(container.ImageID)
		if err != nil {
			return err
		}
		layerID = img.RootFS.ChainID()
	}
	fmt.Println("setRWLayer...", layerID)
	fmt.Println("MountLabel: ", container.MountLabel)
	rwLayer, err := daemon.layerStore.CreateRWLayer(container.ID, layerID, container.MountLabel, daemon.setupInitLayer)
	if err != nil {
		return err
	}
	container.RWLayer = rwLayer

	return nil
}

// VolumeCreate creates a volume with the specified name, driver, and opts
// This is called directly from the remote API
func (daemon *Daemon) VolumeCreate(name, driverName string, opts map[string]string) (*types.Volume, error) {
	if name == "" {
		name = stringid.GenerateNonCryptoID()
	}

	v, err := daemon.volumes.Create(name, driverName, opts)
	if err != nil {
		if volumestore.IsNameConflict(err) {
			return nil, fmt.Errorf("A volume named %s already exists. Choose a different volume name.", name)
		}
		return nil, err
	}

	daemon.LogVolumeEvent(v.Name(), "create", map[string]string{"driver": v.DriverName()})
	return volumeToAPIType(v), nil
}

func compose(paras []string) (string, error) {

	// function2layerdb => map
	manifestP := path.Join("/var/lib/docker/image/aufs/imagedb", "manifest", paras[0])
	fmt.Println(manifestP)
	manifestF, err := os.Open(manifestP)
	if err != nil {
		fmt.Println("open manifest file error")
		return "", err
	}
	defer manifestF.Close()

	functionM := make(map[string]string)
	s := bufio.NewScanner(manifestF)
	for s.Scan() {
		if t := s.Text(); t != "" {
			m := strings.Split(t, ",")
			functionM[m[0]] = m[1]
		}
	}
	fmt.Println(functionM)

	// paras to be composed => slice
	composeS := make([]string, 0, 5)
	paras = append(paras, "chainID") // chainID correspond to reserved top layer
	for _, para := range paras {
		layerdbP := path.Join("/var/lib/docker/image/aufs/layerdb/sha256", functionM[para], "cache-id")
		layerF, err := os.Open(layerdbP)
		if err != nil {
			fmt.Println("open layerdb file error")
			return "", err
		}
		defer layerF.Close()

		fd, err := ioutil.ReadAll(layerF)
		composeS = append(composeS, strings.Replace(string(fd), "\n", "", -1)) // remove "\n"
	}
	fmt.Println(composeS)

	// backup and replace origin layers file
	layersP := path.Join("/var/lib/docker/aufs/layers", composeS[len(composeS)-1])
	ids, err := getParentIds(layersP)
	if err != nil {
		fmt.Println("get parent ids error", err)
		return "", err
	}
	err = os.Rename(layersP, strings.Join([]string{layersP, "-backup"}, ""))
	if err != nil {
		fmt.Println("backup error", err)
		return "", err
	}
	replaceF, err := os.Create(layersP)
	if err != nil {
		fmt.Println("replace error", err)
		return "", err
	}
	defer replaceF.Close()

	// write layers to be composed
	length := len(composeS)
	for i := 1; i < length; i++ {
		if _, err := fmt.Fprintln(replaceF, composeS[length-1-i]); err != nil {
			fmt.Println("replace error", err)
			return "", err
		}
	}
	// write parent layers
	length = len(ids)
	for i := 0; i < length; i++ {
		if ids[i] != composeS[0] {
			continue
		} else {
			for i = i + 1; i < length; i++ {
				if _, err := fmt.Fprintln(replaceF, ids[i]); err != nil {
					fmt.Println("replace error", err)
					return "", err
				}
			}
		}
	}

	return layersP, nil
}

// restore layers file
func restore(layersP string) error {
	if err := os.Remove(layersP); err != nil {
		fmt.Println("restore error", err)
		return err
	}
	err := os.Rename(strings.Join([]string{layersP, "-backup"}, ""), layersP)
	if err != nil {
		fmt.Println("restore error", err)
		return err
	}
	return nil
}

func getParentIds(layersP string) ([]string, error) {
	f, err := os.Open(layersP)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := []string{}
	s := bufio.NewScanner(f)

	for s.Scan() {
		if t := s.Text(); t != "" {
			out = append(out, s.Text())
		}
	}
	return out, s.Err()
}
