package kube

import (
	"context"
	"net"
)

// NodeStats reads only /stats/summary on the kubelet's fixed HTTPS port.
// It uses the API client's verified TLS transport and rotating token, but keeps
// kubelet failures out of API-server health. RBAC needs get nodes/stats, never
// nodes/proxy (which would also authorize container execution).
func (c *Client) NodeStats(ctx context.Context, address string, out any) error {
	ip := net.ParseIP(address)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return &Error{Code: CodeInvalid}
	}
	client := &Client{BaseURL: "https://" + net.JoinHostPort(ip.String(), "10250"), TokenFile: c.TokenFile, HTTP: c.HTTP, UserAgent: c.UserAgent}
	return client.Get(ctx, "/stats/summary", out)
}
