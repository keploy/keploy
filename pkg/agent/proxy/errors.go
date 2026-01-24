package proxy

import (
	"fmt"
	"strings"
)

// ProxyErrorCode represents different types of proxy errors
type ProxyErrorCode string

const (
	ErrCodePortInUse           ProxyErrorCode = "PORT_IN_USE"
	ErrCodePermissionDenied    ProxyErrorCode = "PERMISSION_DENIED"
	ErrCodeNetworkUnreachable  ProxyErrorCode = "NETWORK_UNREACHABLE"
	ErrCodeAddressNotAvailable ProxyErrorCode = "ADDRESS_NOT_AVAILABLE"
	ErrCodeUnknown             ProxyErrorCode = "UNKNOWN"
)

// ProxyError represents a structured error for proxy operations
type ProxyError struct {
	Code      ProxyErrorCode
	Component string // "DNS", "TCP Proxy", "UDP Proxy", "Incoming Proxy"
	Port      uint32
	Message   string
	Hint      string
	Solutions []string
	Err       error // Original error
}

func (e *ProxyError) Error() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("\n❌ Failed to start %s on port %d\n", e.Component, e.Port))
	sb.WriteString(fmt.Sprintf("Error: %s\n", e.Message))

	if e.Hint != "" {
		sb.WriteString(fmt.Sprintf("\n💡 Hint: %s\n", e.Hint))
	}

	if len(e.Solutions) > 0 {
		sb.WriteString("\n🔧 Possible solutions:\n")
		for i, solution := range e.Solutions {
			sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, solution))
		}
	}

	if e.Err != nil {
		sb.WriteString(fmt.Sprintf("\nOriginal error: %v\n", e.Err))
	}

	return sb.String()
}

// Unwrap returns the original error for error chain compatibility
func (e *ProxyError) Unwrap() error {
	return e.Err
}

// NewProxyError creates a ProxyError by analyzing the underlying network error
func NewProxyError(component string, port uint32, err error) *ProxyError {
	if err == nil {
		return nil
	}

	pe := &ProxyError{
		Component: component,
		Port:      port,
		Err:       err,
	}

	errStr := strings.ToLower(err.Error())

	// Detect error type and provide contextual help
	switch {
	case strings.Contains(errStr, "address already in use") || strings.Contains(errStr, "bind: address already in use"):
		pe.Code = ErrCodePortInUse
		pe.Message = fmt.Sprintf("Port %d is already in use by another process", port)
		pe.Hint = "Another instance of Keploy or another application is using this port"
		pe.Solutions = []string{
			fmt.Sprintf("Check what's using the port: sudo lsof -i :%d", port),
			fmt.Sprintf("Kill the process: sudo lsof -t -i :%d | xargs -r kill", port),
			fmt.Sprintf("Use a different port by specifying the appropriate port configuration flag"),
			"Wait a moment and try again (port might be in TIME_WAIT state)",
		}

	case strings.Contains(errStr, "permission denied") || strings.Contains(errStr, "bind: permission denied"):
		pe.Code = ErrCodePermissionDenied
		pe.Message = fmt.Sprintf("Permission denied to bind to port %d", port)
		if port < 1024 {
			pe.Hint = fmt.Sprintf("Port %d is a privileged port (< 1024) and requires elevated privileges", port)
			pe.Solutions = []string{
				"Run Keploy with sudo: sudo keploy record -c \"your-app-command\"",
				fmt.Sprintf("Use an unprivileged port (> 1024) by configuring the appropriate port flag"),
				"Grant CAP_NET_BIND_SERVICE capability: sudo setcap cap_net_bind_service=+ep /path/to/keploy",
			}
		} else {
			pe.Hint = "Insufficient permissions to bind to this port"
			pe.Solutions = []string{
				"Run Keploy with sudo: sudo keploy record -c \"your-app-command\"",
				"Check if a firewall or security policy is blocking the port",
				fmt.Sprintf("Try a different port using the appropriate port configuration flag"),
			}
		}

	case strings.Contains(errStr, "cannot assign requested address") || strings.Contains(errStr, "bind: cannot assign requested address"):
		pe.Code = ErrCodeAddressNotAvailable
		pe.Message = "Cannot assign the requested address"
		pe.Hint = "The specified network interface or address is not available on this system"
		pe.Solutions = []string{
			"Check available network interfaces: ip addr show or ifconfig",
			"Ensure the system's network configuration is correct",
			"If using Docker, check the container's network settings",
			"Try binding to 0.0.0.0 instead of a specific IP",
		}

	case strings.Contains(errStr, "network is unreachable"):
		pe.Code = ErrCodeNetworkUnreachable
		pe.Message = "Network is unreachable"
		pe.Hint = "The system cannot reach the network required for the proxy"
		pe.Solutions = []string{
			"Check network connectivity: ping 8.8.8.8",
			"Verify network interface is up: ip link show",
			"Check routing table: ip route show",
			"Restart network services or try again after network stabilizes",
		}

	default:
		pe.Code = ErrCodeUnknown
		pe.Message = err.Error()
		pe.Hint = "An unexpected error occurred while starting the proxy"
		pe.Solutions = []string{
			fmt.Sprintf("Check if port %d is available: netstat -tlnp | grep %d", port, port),
			"Check system logs for more details: dmesg | tail",
			"Try running with elevated privileges: sudo",
			"Report this issue at https://github.com/keploy/keploy/issues with full error details",
		}
	}

	return pe
}

// NewDNSProxyError creates a DNS-specific proxy error
func NewDNSProxyError(protocol string, port uint32, err error) *ProxyError {
	component := fmt.Sprintf("%s DNS Server", strings.ToUpper(protocol))
	return NewProxyError(component, port, err)
}

// NewTCPProxyError creates a TCP proxy-specific error
func NewTCPProxyError(port uint32, err error) *ProxyError {
	return NewProxyError("TCP Proxy", port, err)
}
