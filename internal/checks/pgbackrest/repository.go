package pgbackrest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/zekihan/argus/internal/kube"
)

// repositoryIdentity deliberately excludes schedules, retention, resource sizing,
// and credentials. Only a digest is persisted, never backend configuration.
func repositoryIdentity(cluster kube.Cluster, name string) string {
	var repository kube.Repository
	for _, item := range cluster.Spec.Backups.PGBackRest.Repos {
		if item.Name == name {
			repository = item
			break
		}
	}
	destination := map[string]string{"path": "/pgbackrest/" + name, "type": "posix"}
	for _, backend := range []string{"s3", "gcs", "azure"} {
		options := map[string]map[string]string{"s3": repository.S3, "gcs": repository.GCS, "azure": repository.Azure}[backend]
		if options != nil {
			destination["type"] = backend
			for key, value := range options {
				destination[backend+"-"+key] = value
			}
		}
	}
	// PGO applies global options after generated backend settings. Hash the
	// effective destination, so adding an explicit default is not a new repo.
	for _, option := range []string{
		"type", "path", "host", "host-port", "host-user", "host-type", "host-config", "host-config-path", "host-config-include-path",
		"storage-host", "storage-port",
		"s3-bucket", "s3-endpoint", "s3-region", "s3-uri-style",
		"azure-account", "azure-container", "azure-endpoint", "azure-uri-style",
		"gcs-bucket", "gcs-endpoint", "sftp-host", "sftp-host-port", "sftp-host-user",
	} {
		if value, ok := cluster.Spec.Backups.PGBackRest.Global[name+"-"+option]; ok {
			destination[option] = value
		}
	}
	volume := ""
	if repository.Volume != nil {
		for _, status := range cluster.Status.PGBackRest.Repos {
			if status.Name == name {
				volume = status.Volume
			}
		}
	}
	// All values originated in JSON; encoding this reduced representation cannot fail.
	data, _ := json.Marshal(struct {
		Destination map[string]string
		VolumeSpec  any
		Volume      string
	}{destination, repository.Volume, volume})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
