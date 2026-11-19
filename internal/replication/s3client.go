// Package replication pushes bucket changes to another S3 endpoint
// (another hogshead server or any S3-compatible service)
// in an active-passive fashion.
package replication

import (
	"github.com/enetforge/hogshead/internal/s3client"
)

// Target is one destination of a replication rule.
type Target struct {
	Name      string `json:"name"` // unique within the bucket; defaults to the endpoint host
	Enabled   bool   `json:"enabled"`
	Prefix    string `json:"prefix,omitempty"` // only keys with this prefix
	Endpoint  string `json:"endpoint"`         // e.g. https://backup.example.com:9000
	Bucket    string `json:"bucket"`           // destination bucket
	AccessKey string `json:"access_key"`       //nolint:gosec // destination credentials by design
	SecretKey string `json:"secret_key"`       //nolint:gosec
	Region    string `json:"region,omitempty"`
	PathStyle bool   `json:"path_style"`
	// Inherited marks a target that comes from the server-wide rule; it is
	// never stored on a bucket rule and only appears in status output.
	Inherited bool `json:"inherited,omitempty"`
	// SyncConfig (server-wide targets only) also mirrors users, access keys,
	// passwords and replicable settings through the target's admin API, so
	// the credentials must belong to an administrator there.
	SyncConfig bool `json:"sync_config,omitempty"`
	// Cellar marks a target managed by cellar membership: it takes part in
	// synchronous write acknowledgement and is shown under Cellar, not
	// Replication.
	Cellar bool `json:"cellar,omitempty"`
}

// Error is a non-2xx response from the destination.
type Error = s3client.Error

func (t Target) endpoint() s3client.Endpoint {
	return s3client.Endpoint{Endpoint: t.Endpoint, Bucket: t.Bucket, AccessKey: t.AccessKey, SecretKey: t.SecretKey, Region: t.Region, PathStyle: t.PathStyle}
}

func newClient(t Target, origin string) *s3client.Client {
	return s3client.New(t.endpoint()).WithOrigin(origin)
}
