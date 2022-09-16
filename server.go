package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"time"

	"google.golang.org/grpc"
	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

const (
	defaultHealthCheckInterval = time.Duration(60)
)

type HostDevice struct {
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
	Permission    string `json:"permission"`
}

// HostDevicePlugin implements the Kubernetes device plugin API
type HostDevicePluginConfig struct {
	ResourceName        string        `json:"resourceName"`
	SocketName          string        `json:"socketName"`
	HostDevices         []*HostDevice `json:"hostDevices"`
	NumDevices          int           `json:"numDevices"`
	HealthCheckInterval time.Duration `json:"healthCheckInterval"`
}

type HostDevicePlugin struct {
	resourceName        string
	socket              string
	HealthCheckInterval time.Duration
	devs                []*pluginapi.Device

	stop   chan interface{}
	health chan string

	// this device files will be mounted to container
	hostDevices []*HostDevice

	server *grpc.Server
}

// var (
// 	_ pluginapi.DevicePluginServer = &HostDevicePlugin{}
// )

// NewHostDevicePlugin returns an initialized HostDevicePlugin
func NewHostDevicePlugin(config HostDevicePluginConfig) (*HostDevicePlugin, error) {
	var devs = make([]*pluginapi.Device, config.NumDevices)

	for i := range devs {
		devs[i] = &pluginapi.Device{
			ID:     fmt.Sprint(i),
			Health: pluginapi.Healthy,
		}
	}

	HealthCheckInterval := defaultHealthCheckInterval
	if config.HealthCheckInterval > 0 {
		HealthCheckInterval = config.HealthCheckInterval
	}

	return &HostDevicePlugin{
		resourceName:        config.ResourceName,
		socket:              pluginapi.DevicePluginPath + config.SocketName,
		HealthCheckInterval: HealthCheckInterval,

		devs:        devs,
		hostDevices: config.HostDevices,

		stop:   make(chan interface{}),
		health: make(chan string),
	}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}

// Start starts the gRPC server of the device plugin
func (m *HostDevicePlugin) Start() error {
	err := m.cleanup()
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	pluginapi.RegisterDevicePluginServer(m.server, m)

	go m.server.Serve(sock)

	// Wait for server to start by launching a blocking connexion
	conn, err := dial(m.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// Stop stops the gRPC server
func (m *HostDevicePlugin) Stop() error {
	if m.server == nil {
		return nil
	}

	m.server.Stop()
	m.server = nil
	close(m.stop)

	return m.cleanup()
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (m *HostDevicePlugin) Register(kubeletEndpoint, resourceName string) error {
	conn, err := dial(kubeletEndpoint, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: resourceName,
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// ListAndWatch lists devices and update that list according to the health status
func (m *HostDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs})

	for {
		select {
		case <-m.stop:
			return nil
		case health := <-m.health:
			for _, dev := range m.devs {
				dev.Health = health
			}
			s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs})
		}
	}
}

// Allocate which return list of devices.
func (m *HostDevicePlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	log.Println("allocate request:", r)

	ress := make([]*pluginapi.ContainerAllocateResponse, len(r.GetContainerRequests()))

	for i := range r.GetContainerRequests() {
		ds := make([]*pluginapi.DeviceSpec, len(m.hostDevices))
		for j := range m.hostDevices {
			ds[j] = &pluginapi.DeviceSpec{
				HostPath:      m.hostDevices[j].HostPath,
				ContainerPath: m.hostDevices[j].ContainerPath,
				Permissions:   m.hostDevices[j].Permission,
			}
		}
		ress[i] = &pluginapi.ContainerAllocateResponse{
			Devices: ds,
		}
	}

	response := pluginapi.AllocateResponse{
		ContainerResponses: ress,
	}

	log.Println("allocate response: ", response)
	return &response, nil
}

func (m *HostDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired: false,
	}, nil
}

func (m *HostDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (m *HostDevicePlugin) cleanup() error {
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// Serve starts the gRPC server and register the device plugin to Kubelet
func (m *HostDevicePlugin) Serve() error {
	err := m.Start()
	if err != nil {
		log.Printf("Could not start device plugin: %s", err)
		return err
	}
	log.Println("Starting to serve on", m.socket)

	err = m.Register(pluginapi.KubeletSocket, m.resourceName)
	if err != nil {
		log.Printf("Could not register device plugin: %s", err)
		m.Stop()
		return err
	}
	log.Println("Registered device plugin with Kubelet")

	return nil
}
