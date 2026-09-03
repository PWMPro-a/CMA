package containeropsagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/containeropsimage"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

func (s *Server) pullCPADeployImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsDeployRenderRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	imagePulls, err := s.docker.PullCPADeployImages(r.Context(), request)
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"status":     "images_pulled",
		"imagePulls": imagePulls,
	})
}

func (c *DockerClient) PullCPADeployImages(ctx context.Context, request model.ContainerOpsDeployRenderRequest) ([]model.ContainerOpsImagePull, error) {
	if err := validateDeployRenderRequest(request); err != nil {
		return nil, err
	}
	images, err := deployPullImages(request.Manifest, request.AllowCustomImages)
	if err != nil {
		return nil, err
	}
	result := make([]model.ContainerOpsImagePull, 0, len(images))
	for _, image := range images {
		if err := c.pullImage(ctx, image); err != nil {
			return nil, fmt.Errorf("pull image %s: %w", image, err)
		}
		result = append(result, model.ContainerOpsImagePull{
			Image:   image,
			Status:  "pulled",
			Message: "Image pull completed through cpamp-agent.",
		})
	}
	return result, nil
}

// deployPullImages validates every image before contacting Docker. The
// optional argument keeps the helper source-compatible with older callers
// while allowing an explicit custom-image opt-in for new deployments.
func deployPullImages(manifest model.ContainerOpsStackManifest, allowCustom ...bool) ([]string, error) {
	allowCustomImages := len(allowCustom) > 0 && allowCustom[0]
	expectedServices := map[string]string{
		"cpa":   "cli-proxy-api",
		"cpamp": "cpa-manager-plus",
		"agent": "cpamp-agent",
	}
	seenRoles := make(map[string]bool, len(expectedServices))
	seenImages := make(map[string]bool, len(expectedServices))
	images := make([]string, 0, len(expectedServices))
	for _, service := range manifest.Services {
		if !service.IncludeInCompose {
			continue
		}
		expectedService, ok := expectedServices[service.Role]
		if !ok {
			return nil, fmt.Errorf("unsupported deploy service role %q", service.Role)
		}
		if service.Service != expectedService {
			return nil, fmt.Errorf("unsupported %s service name %q", service.Role, service.Service)
		}
		if service.Image == "" {
			return nil, fmt.Errorf("%s deploy image is required", service.Role)
		}
		if !deployImageAllowed(service.Role, service.Image, allowCustomImages) {
			return nil, fmt.Errorf("unsupported %s deploy image %q", service.Role, service.Image)
		}
		seenRoles[service.Role] = true
		if seenImages[service.Image] {
			continue
		}
		seenImages[service.Image] = true
		images = append(images, service.Image)
	}
	for role := range expectedServices {
		if !seenRoles[role] {
			return nil, fmt.Errorf("missing %s deploy service", role)
		}
	}
	return images, nil
}

func deployImageAllowed(role string, image string, allowCustom ...bool) bool {
	allowCustomImages := len(allowCustom) > 0 && allowCustom[0]
	return containeropsimage.Allowed(role, image, allowCustomImages)
}

func (c *DockerClient) pullImage(ctx context.Context, image string) error {
	fromImage, tag := splitImageForPull(image)
	query := url.Values{}
	query.Set("fromImage", fromImage)
	if tag != "" {
		query.Set("tag", tag)
	}
	return c.post(ctx, "/images/create?"+query.Encode(), nil, nil)
}

func splitImageForPull(image string) (string, string) {
	reference := strings.TrimSpace(image)
	if strings.Contains(reference, "@") {
		return reference, ""
	}
	colonIndex := strings.LastIndex(reference, ":")
	slashIndex := strings.LastIndex(reference, "/")
	if colonIndex > slashIndex {
		return reference[:colonIndex], reference[colonIndex+1:]
	}
	return reference, "latest"
}
