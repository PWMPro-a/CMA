package containeropsagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const defaultEgressVerifyURL = "https://api.ipify.org"

func (s *Server) listEgressIPs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.MethodNotAllowed(w)
		return
	}
	inventory, err := inspectEgressIPs(r.Context())
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, inventory)
}

func (s *Server) ensureSourceIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsSourceIPRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := ensureEgressSourceIP(r.Context(), request)
	if err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) removeSourceIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsSourceIPRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := removeEgressSourceIP(r.Context(), request)
	if err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) checkSourceIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsSourceIPRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := checkEgressSourceIP(r.Context(), request)
	if err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func inspectEgressIPs(ctx context.Context) (model.ContainerOpsEgressIPInventory, error) {
	var checks []model.ContainerOpsEgressCheck
	route, routeErr := runEgressCommand(ctx, "ip", "route", "get", "1.1.1.1")
	if routeErr != nil {
		checks = append(checks, egressCheck("warning", "default_route_failed", "Default route could not be detected.", routeErr.Error(), false))
	}
	addresses, addrErr := currentIPv4Addresses(ctx)
	if addrErr != nil {
		checks = append(checks, egressCheck("error", "address_list_failed", "Local IPv4 addresses could not be listed.", addrErr.Error(), true))
	}
	defaultInterface := parseDefaultRouteInterface(route.Output)
	if defaultInterface == "" && routeErr == nil {
		checks = append(checks, egressCheck("warning", "default_interface_empty", "Default route did not include an interface.", strings.TrimSpace(route.Output), false))
	}
	nativeOutboundIP, nativeErr := fetchOutboundIP(ctx, "", defaultEgressVerifyURL)
	if nativeErr != nil {
		checks = append(checks, egressCheck("warning", "native_outbound_check_failed", "Native outbound IP check failed.", nativeErr.Error(), false))
	}
	if len(checks) == 0 {
		checks = append(checks, egressCheck("info", "egress_inventory_ready", "Server network inventory was detected.", defaultInterface, false))
	}
	return model.ContainerOpsEgressIPInventory{
		DefaultInterface: defaultInterface,
		Route:            strings.TrimSpace(route.Output),
		NativeOutboundIP: nativeOutboundIP,
		Addresses:        addresses,
		Checks:           checks,
	}, nil
}

func ensureEgressSourceIP(ctx context.Context, request model.ContainerOpsSourceIPRequest) (model.ContainerOpsSourceIPResult, error) {
	sourceIP, err := normalizeSourceIPv4(request.SourceIP)
	if err != nil {
		return model.ContainerOpsSourceIPResult{}, err
	}
	verifyURL := normalizeVerifyURL(request.VerifyURL)
	result := model.ContainerOpsSourceIPResult{
		SourceIP: sourceIP,
		Status:   "completed",
		Checks:   []model.ContainerOpsEgressCheck{},
		Actions:  []model.ContainerOpsEgressAction{},
	}

	inventory, invErr := inspectEgressIPs(ctx)
	if invErr != nil {
		result.Checks = append(result.Checks, egressCheck("warning", "inventory_failed", "Network inventory check failed.", invErr.Error(), false))
	}
	result.NativeOutboundIP = inventory.NativeOutboundIP

	iface, err := resolveSourceInterface(request.Interface, inventory.DefaultInterface)
	if err != nil {
		return model.ContainerOpsSourceIPResult{}, err
	}
	result.Interface = iface

	addresses := inventory.Addresses
	if len(addresses) == 0 {
		addresses, _ = currentIPv4Addresses(ctx)
	}
	if present, actualInterface := hasSourceIP(addresses, sourceIP); present {
		result.Mounted = true
		result.AlreadyPresent = true
		if actualInterface != "" {
			result.Interface = actualInterface
		}
		result.Checks = append(result.Checks, egressCheck("info", "source_ip_already_mounted", "Source IP is already mounted on the server.", result.Interface, false))
	} else {
		action := runEgressAction(ctx, 1, "mount_source_ip", "ip addr add "+sourceIP+"/32 dev "+iface, "ip", "addr", "add", sourceIP+"/32", "dev", iface)
		result.Actions = append(result.Actions, action)
		if action.Status != "completed" {
			result.Status = "failed"
			result.Checks = append(result.Checks, egressCheck("error", "source_ip_mount_failed", "Source IP mount command failed.", action.Message, true))
			return result, nil
		}
		result.Mounted = true
		result.Checks = append(result.Checks, egressCheck("info", "source_ip_mounted", "Source IP was mounted on the server.", iface, false))
	}

	addresses, addrErr := currentIPv4Addresses(ctx)
	if addrErr != nil {
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "address_verify_failed", "Mounted address could not be verified.", addrErr.Error(), true))
		return result, nil
	}
	if present, actualInterface := hasSourceIP(addresses, sourceIP); !present {
		result.Mounted = false
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_not_visible", "Source IP is not visible after mount.", sourceIP, true))
		return result, nil
	} else if actualInterface != "" {
		result.Interface = actualInterface
	}

	outboundIP, outboundErr := fetchOutboundIP(ctx, sourceIP, verifyURL)
	if outboundErr != nil {
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_outbound_failed", "Outbound check through Source IP failed.", outboundErr.Error(), true))
		return result, nil
	}
	result.OutboundIP = outboundIP
	if outboundIP != sourceIP {
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_outbound_mismatch", "Outbound IP does not match the selected Source IP.", outboundIP, true))
		return result, nil
	}
	result.Checks = append(result.Checks, egressCheck("info", "source_ip_outbound_verified", "Outbound IP matches the selected Source IP.", outboundIP, false))
	return result, nil
}

func checkEgressSourceIP(ctx context.Context, request model.ContainerOpsSourceIPRequest) (model.ContainerOpsSourceIPResult, error) {
	sourceIP, err := normalizeSourceIPv4(request.SourceIP)
	if err != nil {
		return model.ContainerOpsSourceIPResult{}, err
	}
	verifyURL := normalizeVerifyURL(request.VerifyURL)
	inventory, invErr := inspectEgressIPs(ctx)
	result := model.ContainerOpsSourceIPResult{
		SourceIP:         sourceIP,
		Status:           "completed",
		NativeOutboundIP: inventory.NativeOutboundIP,
		Checks:           []model.ContainerOpsEgressCheck{},
	}
	if invErr != nil {
		result.Checks = append(result.Checks, egressCheck("warning", "inventory_failed", "Network inventory check failed.", invErr.Error(), false))
	}
	if present, iface := hasSourceIP(inventory.Addresses, sourceIP); present {
		result.Mounted = true
		result.Interface = iface
		result.Checks = append(result.Checks, egressCheck("info", "source_ip_mounted", "Source IP is mounted on the server.", iface, false))
	} else {
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_not_mounted", "Source IP is not mounted on the server.", sourceIP, true))
		return result, nil
	}
	outboundIP, outboundErr := fetchOutboundIP(ctx, sourceIP, verifyURL)
	if outboundErr != nil {
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_outbound_failed", "Outbound check through Source IP failed.", outboundErr.Error(), true))
		return result, nil
	}
	result.OutboundIP = outboundIP
	if outboundIP != sourceIP {
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_outbound_mismatch", "Outbound IP does not match the selected Source IP.", outboundIP, true))
		return result, nil
	}
	result.Checks = append(result.Checks, egressCheck("info", "source_ip_outbound_verified", "Outbound IP matches the selected Source IP.", outboundIP, false))
	return result, nil
}

func removeEgressSourceIP(ctx context.Context, request model.ContainerOpsSourceIPRequest) (model.ContainerOpsSourceIPResult, error) {
	sourceIP, err := normalizeSourceIPv4(request.SourceIP)
	if err != nil {
		return model.ContainerOpsSourceIPResult{}, err
	}
	inventory, invErr := inspectEgressIPs(ctx)
	result := model.ContainerOpsSourceIPResult{
		SourceIP:         sourceIP,
		Status:           "completed",
		NativeOutboundIP: inventory.NativeOutboundIP,
		Checks:           []model.ContainerOpsEgressCheck{},
		Actions:          []model.ContainerOpsEgressAction{},
	}
	if invErr != nil {
		result.Checks = append(result.Checks, egressCheck("warning", "inventory_failed", "Network inventory check failed.", invErr.Error(), false))
	}
	requestInterface := strings.TrimSpace(request.Interface)
	present, actualInterface := hasSourceIP(inventory.Addresses, sourceIP)
	if !present {
		result.Checks = append(result.Checks, egressCheck("info", "source_ip_already_removed", "Source IP is already absent from the server.", sourceIP, false))
		return result, nil
	}
	iface, err := resolveSourceInterface(requestInterface, actualInterface)
	if err != nil {
		return model.ContainerOpsSourceIPResult{}, err
	}
	result.Interface = iface
	action := runEgressAction(ctx, 1, "remove_source_ip", "ip addr del "+sourceIP+"/32 dev "+iface, "ip", "addr", "del", sourceIP+"/32", "dev", iface)
	result.Actions = append(result.Actions, action)
	if action.Status != "completed" {
		result.Status = "failed"
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_remove_failed", "Source IP remove command failed.", action.Message, true))
		return result, nil
	}
	addresses, addrErr := currentIPv4Addresses(ctx)
	if addrErr != nil {
		result.Status = "blocked"
		result.Checks = append(result.Checks, egressCheck("error", "address_verify_failed", "Mounted address list could not be verified.", addrErr.Error(), true))
		return result, nil
	}
	if present, _ := hasSourceIP(addresses, sourceIP); present {
		result.Status = "blocked"
		result.Mounted = true
		result.Checks = append(result.Checks, egressCheck("error", "source_ip_still_visible", "Source IP is still visible after remove.", sourceIP, true))
		return result, nil
	}
	result.Removed = true
	result.Checks = append(result.Checks, egressCheck("info", "source_ip_removed", "Source IP was removed from the server.", iface, false))
	return result, nil
}

type egressCommandOutput struct {
	Output string
}

func currentIPv4Addresses(ctx context.Context) ([]model.ContainerOpsEgressAddress, error) {
	output, err := runEgressCommand(ctx, "ip", "-o", "-4", "addr", "show")
	if err != nil {
		return nil, err
	}
	return parseIPv4AddressLines(output.Output), nil
}

func runEgressAction(ctx context.Context, order int, code string, target string, name string, args ...string) model.ContainerOpsEgressAction {
	output, err := runEgressCommand(ctx, name, args...)
	action := model.ContainerOpsEgressAction{
		Order:  order,
		Code:   code,
		Target: target,
		Status: "completed",
		Output: strings.TrimSpace(output.Output),
	}
	if err != nil {
		action.Status = "failed"
		action.Message = err.Error()
	}
	return action
}

func runEgressCommand(ctx context.Context, name string, args ...string) (egressCommandOutput, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	raw, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(raw))
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return egressCommandOutput{Output: output}, fmt.Errorf("%s timed out", strings.Join(append([]string{name}, args...), " "))
	}
	if err != nil {
		if output == "" {
			output = err.Error()
		}
		return egressCommandOutput{Output: output}, fmt.Errorf("%s: %s", strings.Join(append([]string{name}, args...), " "), output)
	}
	return egressCommandOutput{Output: output}, nil
}

func parseDefaultRouteInterface(output string) string {
	fields := strings.Fields(output)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "dev" {
			return cleanInterfaceName(fields[i+1])
		}
	}
	return ""
}

func parseIPv4AddressLines(output string) []model.ContainerOpsEgressAddress {
	lines := strings.Split(output, "\n")
	addresses := make([]model.ContainerOpsEgressAddress, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		iface := cleanInterfaceName(strings.TrimSuffix(fields[1], ":"))
		cidr := ""
		scope := ""
		for i := 2; i < len(fields); i++ {
			switch fields[i] {
			case "inet":
				if i+1 < len(fields) {
					cidr = fields[i+1]
				}
			case "scope":
				if i+1 < len(fields) {
					scope = fields[i+1]
				}
			}
		}
		if cidr == "" {
			continue
		}
		address := cidr
		if slash := strings.IndexByte(cidr, '/'); slash >= 0 {
			address = cidr[:slash]
		}
		if net.ParseIP(address).To4() == nil {
			continue
		}
		addresses = append(addresses, model.ContainerOpsEgressAddress{
			Interface: iface,
			Address:   address,
			CIDR:      cidr,
			Scope:     scope,
		})
	}
	return addresses
}

func hasSourceIP(addresses []model.ContainerOpsEgressAddress, sourceIP string) (bool, string) {
	for _, address := range addresses {
		if address.Address == sourceIP {
			return true, address.Interface
		}
	}
	return false, ""
}

func resolveSourceInterface(requested string, fallback string) (string, error) {
	iface := firstNonEmptyValue(strings.TrimSpace(requested), strings.TrimSpace(fallback))
	if iface == "" {
		return "", errors.New("source interface is required")
	}
	if !isSafeInterfaceName(iface) {
		return "", fmt.Errorf("invalid source interface %q", iface)
	}
	return cleanInterfaceName(iface), nil
}

func normalizeSourceIPv4(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "/") {
		return "", errors.New("sourceIp must be a plain IPv4 address")
	}
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil {
		return "", errors.New("sourceIp must be a valid IPv4 address")
	}
	ip = ip.To4()
	if ip.IsUnspecified() || ip.IsMulticast() {
		return "", errors.New("sourceIp must be a usable unicast IPv4 address")
	}
	return ip.String(), nil
}

func normalizeVerifyURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultEgressVerifyURL
	}
	return value
}

func cleanInterfaceName(value string) string {
	value = strings.TrimSpace(value)
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	return strings.TrimSuffix(value, ":")
}

func isSafeInterfaceName(value string) bool {
	value = cleanInterfaceName(value)
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func fetchOutboundIP(ctx context.Context, sourceIP string, verifyURL string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(sourceIP))
	dialer := &net.Dialer{Timeout: 6 * time.Second}
	if sourceIP != "" {
		if ip == nil || ip.To4() == nil {
			return "", errors.New("source IP is invalid")
		}
		dialer.LocalAddr = &net.TCPAddr{IP: ip.To4()}
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   6 * time.Second,
			ResponseHeaderTimeout: 6 * time.Second,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "cpamp-agent-egress-check/1.0")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("verify endpoint returned HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 512))
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(string(raw))
	fields := strings.Fields(strings.Trim(body, "\""))
	if len(fields) == 0 {
		return "", errors.New("verify endpoint returned an empty body")
	}
	outboundIP := strings.Trim(fields[0], "\"")
	if net.ParseIP(outboundIP).To4() == nil {
		return "", fmt.Errorf("verify endpoint returned %q", body)
	}
	return outboundIP, nil
}

func egressCheck(severity string, code string, message string, resource string, blocking bool) model.ContainerOpsEgressCheck {
	return model.ContainerOpsEgressCheck{
		Severity: severity,
		Code:     code,
		Message:  message,
		Resource: resource,
		Blocking: blocking,
	}
}
