package containeropsagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	defaultDockerHost = "unix:///var/run/docker.sock"
	managedLabelKey   = "com.cpamp.managed"
)

type DockerClient struct {
	host       string
	socketPath string
	client     *http.Client
}

func NewDockerClient(host string) (*DockerClient, error) {
	if strings.TrimSpace(host) == "" {
		host = defaultDockerHost
	}
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse docker host: %w", err)
	}
	if parsed.Scheme != "unix" {
		return nil, fmt.Errorf("unsupported docker host scheme %q", parsed.Scheme)
	}
	socketPath := parsed.Path
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: 5 * time.Second}
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &DockerClient{
		host:       host,
		socketPath: socketPath,
		client:     &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}, nil
}

func (c *DockerClient) Host() string {
	return c.host
}

func (c *DockerClient) Overview(ctx context.Context) (model.ContainerOpsDockerOverview, error) {
	var containers []dockerContainer
	if err := c.get(ctx, "/containers/json?all=1", &containers); err != nil {
		return model.ContainerOpsDockerOverview{}, fmt.Errorf("list containers: %w", err)
	}
	var networks []dockerNetwork
	if err := c.get(ctx, "/networks", &networks); err != nil {
		return model.ContainerOpsDockerOverview{}, fmt.Errorf("list networks: %w", err)
	}
	var images []dockerImage
	if err := c.get(ctx, "/images/json", &images); err != nil {
		return model.ContainerOpsDockerOverview{}, fmt.Errorf("list images: %w", err)
	}
	return buildOverview(containers, networks, images), nil
}

func (c *DockerClient) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("docker api status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return err
	}
	return nil
}

func (c *DockerClient) post(ctx context.Context, path string, body any, target any) error {
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+path, &payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("docker api status %d", resp.StatusCode)
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return err
	}
	return nil
}

type dockerContainer struct {
	ID              string                         `json:"Id"`
	Names           []string                       `json:"Names"`
	Image           string                         `json:"Image"`
	ImageID         string                         `json:"ImageID"`
	State           string                         `json:"State"`
	Status          string                         `json:"Status"`
	Labels          map[string]string              `json:"Labels"`
	Ports           []dockerPort                   `json:"Ports"`
	Mounts          []dockerMount                  `json:"Mounts"`
	NetworkSettings dockerContainerNetworkSettings `json:"NetworkSettings"`
}

type dockerPort struct {
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
	IP          string `json:"IP"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

type dockerContainerNetworkSettings struct {
	Networks map[string]dockerEndpoint `json:"Networks"`
}

type dockerEndpoint struct {
	NetworkID string `json:"NetworkID"`
	IPAddress string `json:"IPAddress"`
	Gateway   string `json:"Gateway"`
}

type dockerNetwork struct {
	ID         string                    `json:"Id"`
	Name       string                    `json:"Name"`
	Driver     string                    `json:"Driver"`
	Scope      string                    `json:"Scope"`
	Internal   bool                      `json:"Internal"`
	Attachable bool                      `json:"Attachable"`
	Labels     map[string]string         `json:"Labels"`
	Containers map[string]dockerEndpoint `json:"Containers"`
}

type dockerImage struct {
	ID       string            `json:"Id"`
	RepoTags []string          `json:"RepoTags"`
	Size     int64             `json:"Size"`
	Created  int64             `json:"Created"`
	Labels   map[string]string `json:"Labels"`
}

func buildOverview(containers []dockerContainer, networks []dockerNetwork, images []dockerImage) model.ContainerOpsDockerOverview {
	result := model.ContainerOpsDockerOverview{
		Containers: make([]model.ContainerOpsDockerContainer, 0, len(containers)),
		Networks:   make([]model.ContainerOpsDockerNetwork, 0, len(networks)),
		Images:     make([]model.ContainerOpsDockerImage, 0, len(images)),
	}
	for _, item := range containers {
		container := convertContainer(item)
		result.Containers = append(result.Containers, container)
		result.Summary.ContainerCount++
		if container.State == "running" {
			result.Summary.RunningCount++
		}
		if container.Managed {
			result.Summary.ManagedCount++
		}
		switch container.Role {
		case "cpa":
			result.Summary.CPACount++
		case "cpamp":
			result.Summary.CPAMPCount++
		case "newapi":
			result.Summary.NewAPICount++
		}
	}
	for _, item := range networks {
		result.Networks = append(result.Networks, convertNetwork(item))
	}
	for _, item := range images {
		result.Images = append(result.Images, convertImage(item))
	}
	result.Summary.NetworkCount = len(result.Networks)
	result.Summary.ImageCount = len(result.Images)
	sort.Slice(result.Containers, func(i, j int) bool {
		return result.Containers[i].Name < result.Containers[j].Name
	})
	sort.Slice(result.Networks, func(i, j int) bool {
		return result.Networks[i].Name < result.Networks[j].Name
	})
	sort.Slice(result.Images, func(i, j int) bool {
		return strings.Join(result.Images[i].RepoTags, ",") < strings.Join(result.Images[j].RepoTags, ",")
	})
	return result
}

func convertContainer(item dockerContainer) model.ContainerOpsDockerContainer {
	name := normalizeContainerName(item.Names)
	container := model.ContainerOpsDockerContainer{
		ID:       shortID(item.ID),
		Name:     name,
		Image:    item.Image,
		ImageID:  shortID(item.ImageID),
		State:    item.State,
		Status:   item.Status,
		Role:     detectRole(name, item.Image, item.Labels),
		Managed:  isManaged(item.Labels),
		Labels:   item.Labels,
		Ports:    make([]model.ContainerOpsDockerPort, 0, len(item.Ports)),
		Mounts:   make([]model.ContainerOpsDockerMount, 0, len(item.Mounts)),
		Networks: make([]model.ContainerOpsDockerAttachment, 0, len(item.NetworkSettings.Networks)),
	}
	for _, port := range item.Ports {
		container.Ports = append(container.Ports, model.ContainerOpsDockerPort{
			PrivatePort: port.PrivatePort,
			PublicPort:  port.PublicPort,
			Type:        port.Type,
			IP:          port.IP,
		})
	}
	for _, mount := range item.Mounts {
		container.Mounts = append(container.Mounts, model.ContainerOpsDockerMount{
			Type:        mount.Type,
			Name:        mount.Name,
			Source:      mount.Source,
			Destination: mount.Destination,
			Mode:        mount.Mode,
			RW:          mount.RW,
		})
	}
	for networkName, endpoint := range item.NetworkSettings.Networks {
		container.Networks = append(container.Networks, model.ContainerOpsDockerAttachment{
			Name:      networkName,
			NetworkID: shortID(endpoint.NetworkID),
			IPAddress: endpoint.IPAddress,
			Gateway:   endpoint.Gateway,
		})
	}
	sort.Slice(container.Networks, func(i, j int) bool {
		return container.Networks[i].Name < container.Networks[j].Name
	})
	return container
}

func convertNetwork(item dockerNetwork) model.ContainerOpsDockerNetwork {
	return model.ContainerOpsDockerNetwork{
		ID:         shortID(item.ID),
		Name:       item.Name,
		Driver:     item.Driver,
		Scope:      item.Scope,
		Internal:   item.Internal,
		Attachable: item.Attachable,
		Managed:    isManaged(item.Labels),
		Labels:     item.Labels,
		Containers: len(item.Containers),
	}
}

func convertImage(item dockerImage) model.ContainerOpsDockerImage {
	return model.ContainerOpsDockerImage{
		ID:       shortID(item.ID),
		RepoTags: item.RepoTags,
		Size:     item.Size,
		Created:  item.Created,
		Labels:   item.Labels,
	}
}

func normalizeContainerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	name := strings.TrimPrefix(names[0], "/")
	return strings.TrimSpace(name)
}

func detectRole(name string, image string, labels map[string]string) string {
	if role := strings.TrimSpace(labels["com.cpamp.role"]); role != "" {
		return role
	}
	probe := strings.ToLower(name + " " + image)
	switch {
	case strings.Contains(probe, "cli-proxy-api") || strings.Contains(probe, "cliproxyapi"):
		return "cpa"
	case strings.Contains(probe, "cpamp-agent"):
		return "agent"
	case strings.Contains(probe, "cpa-manager-plus"):
		return "cpamp"
	case strings.Contains(probe, "new-api") || strings.Contains(probe, "newapi"):
		return "newapi"
	default:
		return ""
	}
}

func isManaged(labels map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(labels[managedLabelKey]), "true")
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
