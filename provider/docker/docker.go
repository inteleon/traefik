package docker

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cenk/backoff"
	"github.com/containous/traefik/job"
	"github.com/containous/traefik/log"
	"github.com/containous/traefik/provider"
	"github.com/containous/traefik/provider/docker/event"
	"github.com/containous/traefik/safe"
	"github.com/containous/traefik/types"
	"github.com/containous/traefik/version"
	dockertypes "github.com/docker/docker/api/types"
	dockercontainertypes "github.com/docker/docker/api/types/container"
	eventtypes "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/docker/go-connections/sockets"
)

const (
	// SwarmAPIVersion is a constant holding the version of the Provider API traefik will use
	SwarmAPIVersion = "1.24"
)

var _ provider.Provider = (*Provider)(nil)

// Provider holds configurations of the provider.
type Provider struct {
	provider.BaseProvider `mapstructure:",squash" export:"true"`
	Endpoint              string           `description:"Docker server endpoint. Can be a tcp or a unix socket endpoint"`
	Domain                string           `description:"Default domain used"`
	TLS                   *types.ClientTLS `description:"Enable Docker TLS support" export:"true"`
	ExposedByDefault      bool             `description:"Expose containers by default" export:"true"`
	UseBindPortIP         bool             `description:"Use the ip address from the bound port, rather than from the inner network" export:"true"`
	SwarmMode             bool             `description:"Use Docker on Swarm Mode" export:"true"`
}

// dockerData holds the need data to the Provider p
type dockerData struct {
	ServiceName     string
	Name            string
	Labels          map[string]string // List of labels set to container or service
	NetworkSettings networkSettings
	Health          string
	Node            *dockertypes.ContainerNode
	SegmentLabels   map[string]string
	SegmentName     string
}

// NetworkSettings holds the networks data to the Provider p
type networkSettings struct {
	NetworkMode dockercontainertypes.NetworkMode
	Ports       nat.PortMap
	Networks    map[string]*networkData
}

// Network holds the network data to the Provider p
type networkData struct {
	Name     string
	Addr     string
	Port     int
	Protocol string
	ID       string
}

func (p *Provider) createClient() (client.APIClient, error) {
	var httpClient *http.Client

	if p.TLS != nil {
		config, err := p.TLS.CreateTLSConfig()
		if err != nil {
			return nil, err
		}
		tr := &http.Transport{
			TLSClientConfig: config,
		}

		hostURL, err := client.ParseHostURL(p.Endpoint)
		if err != nil {
			return nil, err
		}
		sockets.ConfigureTransport(tr, hostURL.Scheme, hostURL.Host)

		httpClient = &http.Client{
			Transport: tr,
		}
	}

	httpHeaders := map[string]string{
		"User-Agent": "Traefik " + version.Version,
	}

	var apiVersion string
	if p.SwarmMode {
		apiVersion = SwarmAPIVersion
	} else {
		apiVersion = DockerAPIVersion
	}

	return client.NewClient(p.Endpoint, apiVersion, httpClient, httpHeaders)
}

// Provide allows the docker provider to provide configurations to traefik
// using the given configuration channel.
func (p *Provider) Provide(configurationChan chan<- types.ConfigMessage, pool *safe.Pool, constraints types.Constraints) error {
	p.Constraints = append(p.Constraints, constraints...)
	// TODO register this routine in pool, and watch for stop channel
	safe.Go(func() {
		operation := func() error {
			var err error

			dockerClient, err := p.createClient()
			if err != nil {
				log.Errorf("Failed to create a client for docker, error: %s", err)
				return err
			}

			ctx := context.Background()
			serverVersion, err := dockerClient.ServerVersion(ctx)
			if err != nil {
				log.Errorf("Failed to retrieve information of the docker client and server host: %s", err)
				return err
			}
			log.Debugf("Provider connection established with docker %s (API %s)", serverVersion.Version, serverVersion.APIVersion)
			var dockerDataList []dockerData
			if p.SwarmMode {
				dockerDataList, err = listServices(ctx, dockerClient)
				if err != nil {
					log.Errorf("Failed to list services for docker swarm mode, error %s", err)
					return err
				}
			} else {
				dockerDataList, err = listContainers(ctx, dockerClient)
				if err != nil {
					log.Errorf("Failed to list containers for docker, error %s", err)
					return err
				}
			}

			configuration := p.buildConfiguration(dockerDataList)
			configurationChan <- types.ConfigMessage{
				ProviderName:  "docker",
				Configuration: configuration,
			}
			if p.Watch {
				if p.SwarmMode {
					errChan := make(chan error)
					pool.Go(func(stop chan bool) {
						watchCtx, cancel := context.WithCancel(ctx)
						defer cancel()

						defer close(errChan)

						// Explicitly define the callbackFunc so we can call it recursively within itself.
						var callbackFunc func(eventtypes.Message)

						callbackFunc = func(msg eventtypes.Message) {
							log.Debugf("Docker events callback function executed with payload: %#v", msg)

							listAndUpdateServicesHelper := func() {
								if err := p.listAndUpdateServices(watchCtx, dockerClient, configurationChan); err != nil {
									log.Errorf("Failed to list services for docker, error %s", err)
								}
							}

							if msg.Actor.ID != "" {
								taskList, err := dockerClient.TaskList(
									watchCtx,
									dockertypes.TaskListOptions{
										Filters: filters.NewArgs(
											filters.Arg("service", msg.Actor.ID),
											filters.Arg("desired-state", "running"),
										),
									},
								)
								if err != nil {
									log.Errorf("Failed to list tasks for service %s, error %s", msg.Actor.ID, err)

									return
								}

								retry := false
								if len(taskList) == 0 {
									retry = true
								}

							TaskLoop:
								for _, task := range taskList {
									log.Debugf("State of task %s: %s", task.ID, task.Status.State)

									if task.Status.State != swarmtypes.TaskStateRunning {
										switch task.Status.State {
										case
											swarmtypes.TaskStateNew,
											swarmtypes.TaskStatePending,
											swarmtypes.TaskStateAssigned,
											swarmtypes.TaskStateAccepted,
											swarmtypes.TaskStatePreparing,
											swarmtypes.TaskStateStarting:
											retry = true

											break TaskLoop
										}
									}
								}

								if !retry {
									log.Debug("Callback task state check: Won't retry")

									listAndUpdateServicesHelper()
								} else {
									log.Debug("Callback task state check: Retrying in 1 second")

									// Sleep 1 second between retries.
									time.Sleep(1 * time.Second)

									log.Debug("Callback task state check: Retrying...")
									callbackFunc(msg)
								}

								return
							}

							listAndUpdateServicesHelper()
						}

						listener, err := event.NewListener(
							dockerClient,
							dockertypes.EventsOptions{
								Filters: filters.NewArgs(
									filters.Arg("scope", "swarm"),
									filters.Arg("type", "service"),
								),
							},
							stop,
							errChan,
							callbackFunc,
						)
						if err != nil {
							log.Errorf("Unable to create a new event listener, error %s", err.Error())
							errChan <- err
							return
						}

						// Blocking.
						listener.Start()
					})
					if err, ok := <-errChan; ok {
						return err
					}
					// channel closed

				} else {
					watchCtx, cancel := context.WithCancel(ctx)
					defer cancel()

					f := filters.NewArgs()
					f.Add("type", "container")
					options := dockertypes.EventsOptions{
						Filters: f,
					}

					startStopHandle := func(m eventtypes.Message) {
						log.Debugf("Provider event received %+v", m)
						containers, err := listContainers(watchCtx, dockerClient)
						if err != nil {
							log.Errorf("Failed to list containers for docker, error %s", err)
							// Call cancel to get out of the monitor
							cancel()
							return
						}
						configuration := p.buildConfiguration(containers)
						if configuration != nil {
							configurationChan <- types.ConfigMessage{
								ProviderName:  "docker",
								Configuration: configuration,
							}
						}
					}

					eventsc, errc := dockerClient.Events(watchCtx, options)
					for {
						select {
						case event := <-eventsc:
							if event.Action == "start" ||
								event.Action == "die" ||
								strings.HasPrefix(event.Action, "health_status") {
								startStopHandle(event)
							}
						case err := <-errc:
							if err == io.EOF {
								log.Debug("Provider event stream closed")
							}

							return err
						}
					}
				}
			}
			return nil
		}
		notify := func(err error, time time.Duration) {
			log.Errorf("Provider connection error %+v, retrying in %s", err, time)
		}
		err := backoff.RetryNotify(safe.OperationWithRecover(operation), job.NewBackOff(backoff.NewExponentialBackOff()), notify)
		if err != nil {
			log.Errorf("Cannot connect to docker server %+v", err)
		}
	})

	return nil
}

func (p *Provider) listAndUpdateServices(ctx context.Context, dockerClient client.APIClient, configurationChan chan<- types.ConfigMessage) error {
	log.Debug("listAndUpdateServices called!")
	services, err := listServices(ctx, dockerClient)
	if err != nil {
		return err
	}
	log.Debugf("Services found! %#v", services)

	configuration := p.buildConfiguration(services)
	log.Debugf("Configuration built: %#v", configuration)
	if configuration != nil {
		configurationChan <- types.ConfigMessage{
			ProviderName:  "docker",
			Configuration: configuration,
		}
	}

	return nil
}

func listContainers(ctx context.Context, dockerClient client.ContainerAPIClient) ([]dockerData, error) {
	containerList, err := dockerClient.ContainerList(ctx, dockertypes.ContainerListOptions{})
	if err != nil {
		return nil, err
	}

	var containersInspected []dockerData
	// get inspect containers
	for _, container := range containerList {
		dData := inspectContainers(ctx, dockerClient, container.ID)
		if len(dData.Name) > 0 {
			containersInspected = append(containersInspected, dData)
		}
	}
	return containersInspected, nil
}

func inspectContainers(ctx context.Context, dockerClient client.ContainerAPIClient, containerID string) dockerData {
	dData := dockerData{}
	containerInspected, err := dockerClient.ContainerInspect(ctx, containerID)
	if err != nil {
		log.Warnf("Failed to inspect container %s, error: %s", containerID, err)
	} else {
		// This condition is here to avoid to have empty IP https://github.com/containous/traefik/issues/2459
		// We register only container which are running
		if containerInspected.ContainerJSONBase != nil && containerInspected.ContainerJSONBase.State != nil && containerInspected.ContainerJSONBase.State.Running {
			dData = parseContainer(containerInspected)
		}
	}
	return dData
}

func parseContainer(container dockertypes.ContainerJSON) dockerData {
	dData := dockerData{
		NetworkSettings: networkSettings{},
	}

	if container.ContainerJSONBase != nil {
		dData.Name = container.ContainerJSONBase.Name
		dData.ServiceName = dData.Name // Default ServiceName to be the container's Name.
		dData.Node = container.ContainerJSONBase.Node

		if container.ContainerJSONBase.HostConfig != nil {
			dData.NetworkSettings.NetworkMode = container.ContainerJSONBase.HostConfig.NetworkMode
		}

		if container.State != nil && container.State.Health != nil {
			dData.Health = container.State.Health.Status
		}
	}

	if container.Config != nil && container.Config.Labels != nil {
		dData.Labels = container.Config.Labels
	}

	if container.NetworkSettings != nil {
		if container.NetworkSettings.Ports != nil {
			dData.NetworkSettings.Ports = container.NetworkSettings.Ports
		}
		if container.NetworkSettings.Networks != nil {
			dData.NetworkSettings.Networks = make(map[string]*networkData)
			for name, containerNetwork := range container.NetworkSettings.Networks {
				dData.NetworkSettings.Networks[name] = &networkData{
					ID:   containerNetwork.NetworkID,
					Name: name,
					Addr: containerNetwork.IPAddress,
				}
			}
		}
	}
	return dData
}

func listServices(ctx context.Context, dockerClient client.APIClient) ([]dockerData, error) {
	serviceList, err := dockerClient.ServiceList(ctx, dockertypes.ServiceListOptions{})
	log.Debugf("Service list: %#v", serviceList)
	if err != nil {
		return nil, err
	}

	serverVersion, err := dockerClient.ServerVersion(ctx)
	if err != nil {
		return nil, err
	}

	networkListArgs := filters.NewArgs()
	// https://docs.docker.com/engine/api/v1.29/#tag/Network (Docker 17.06)
	if versions.GreaterThanOrEqualTo(serverVersion.APIVersion, "1.29") {
		networkListArgs.Add("scope", "swarm")
	} else {
		networkListArgs.Add("driver", "overlay")
	}

	networkList, err := dockerClient.NetworkList(ctx, dockertypes.NetworkListOptions{Filters: networkListArgs})
	if err != nil {
		log.Debugf("Failed to network inspect on client for docker, error: %s", err)
		return nil, err
	}

	networkMap := make(map[string]*dockertypes.NetworkResource)
	for _, network := range networkList {
		networkToAdd := network
		networkMap[network.ID] = &networkToAdd
	}

	var dockerDataList []dockerData
	var dockerDataListTasks []dockerData

	for _, service := range serviceList {
		dData := parseService(service, networkMap)

		if isBackendLBSwarm(dData) {
			if len(dData.NetworkSettings.Networks) > 0 {
				dockerDataList = append(dockerDataList, dData)
			} else {
				log.Warnf("No network found for service %s", service.Spec.Name)
			}
		} else {
			isGlobalSvc := service.Spec.Mode.Global != nil
			dockerDataListTasks, err = listTasks(ctx, dockerClient, service.ID, dData, networkMap, isGlobalSvc)
			if err != nil {
				log.Warnf("No tasks found for service %s, error %s", service.Spec.Name, err.Error())
			} else {
				log.Debugf("Tasks for service %s: %#v", service.Spec.Name, dockerDataListTasks)
				dockerDataList = append(dockerDataList, dockerDataListTasks...)
			}
		}
	}

	return dockerDataList, err
}

func parseService(service swarmtypes.Service, networkMap map[string]*dockertypes.NetworkResource) dockerData {
	dData := dockerData{
		ServiceName:     service.Spec.Annotations.Name,
		Name:            service.Spec.Annotations.Name,
		Labels:          service.Spec.Annotations.Labels,
		NetworkSettings: networkSettings{},
	}

	if service.Spec.EndpointSpec != nil {
		if service.Spec.EndpointSpec.Mode == swarmtypes.ResolutionModeDNSRR {
			if isBackendLBSwarm(dData) {
				log.Warnf("Ignored %s endpoint-mode not supported, service name: %s. Fallback to Træfik load balancing", swarmtypes.ResolutionModeDNSRR, service.Spec.Annotations.Name)
			}
		} else if service.Spec.EndpointSpec.Mode == swarmtypes.ResolutionModeVIP {
			dData.NetworkSettings.Networks = make(map[string]*networkData)
			for _, virtualIP := range service.Endpoint.VirtualIPs {
				networkService := networkMap[virtualIP.NetworkID]
				if networkService != nil {
					if len(virtualIP.Addr) > 0 {
						ip, _, _ := net.ParseCIDR(virtualIP.Addr)
						network := &networkData{
							Name: networkService.Name,
							ID:   virtualIP.NetworkID,
							Addr: ip.String(),
						}
						dData.NetworkSettings.Networks[network.Name] = network
					} else {
						log.Debugf("No virtual IPs found in network %s", virtualIP.NetworkID)
					}
				} else {
					log.Debugf("Network not found, id: %s", virtualIP.NetworkID)
				}
			}
		}
	}
	return dData
}

func listTasks(ctx context.Context, dockerClient client.APIClient, serviceID string,
	serviceDockerData dockerData, networkMap map[string]*dockertypes.NetworkResource, isGlobalSvc bool) ([]dockerData, error) {
	serviceIDFilter := filters.NewArgs()
	serviceIDFilter.Add("service", serviceID)
	serviceIDFilter.Add("desired-state", "running")

	taskList, err := dockerClient.TaskList(ctx, dockertypes.TaskListOptions{Filters: serviceIDFilter})
	if err != nil {
		return nil, err
	}

	var dockerDataList []dockerData
	for _, task := range taskList {
		if task.Status.State != swarmtypes.TaskStateRunning {
			log.Warnf(
				"Task %s is not in the desired state (current state: %s, desired state: %s, service: %s)",
				task.ID,
				task.Status.State,
				swarmtypes.TaskStateRunning,
				serviceID,
			)

			continue
		}
		dData := parseTasks(task, serviceDockerData, networkMap, isGlobalSvc)
		if len(dData.NetworkSettings.Networks) > 0 {
			dockerDataList = append(dockerDataList, dData)
		} else {
			log.Warnf("No networks found for task %s (service: %s)", task.ID, serviceID)
		}
	}
	return dockerDataList, err
}

func parseTasks(task swarmtypes.Task, serviceDockerData dockerData,
	networkMap map[string]*dockertypes.NetworkResource, isGlobalSvc bool) dockerData {
	dData := dockerData{
		ServiceName:     serviceDockerData.Name,
		Name:            serviceDockerData.Name + "." + strconv.Itoa(task.Slot),
		Labels:          serviceDockerData.Labels,
		NetworkSettings: networkSettings{},
	}

	if isGlobalSvc {
		dData.Name = serviceDockerData.Name + "." + task.ID
	}

	if task.NetworksAttachments != nil {
		dData.NetworkSettings.Networks = make(map[string]*networkData)
		for _, virtualIP := range task.NetworksAttachments {
			if networkService, present := networkMap[virtualIP.Network.ID]; present {
				if len(virtualIP.Addresses) > 0 {
					// Not sure about this next loop - when would a task have multiple IP's for the same network?
					for _, addr := range virtualIP.Addresses {
						ip, _, _ := net.ParseCIDR(addr)
						network := &networkData{
							ID:   virtualIP.Network.ID,
							Name: networkService.Name,
							Addr: ip.String(),
						}
						dData.NetworkSettings.Networks[network.Name] = network
					}
				} else {
					log.Debugf("No IP addresses found for network %s", virtualIP.Network.ID)
				}
			}
		}
	}
	return dData
}
