package kube

const Prefix = "postgres-operator.crunchydata.com/"
const ClusterAPI = "postgres-operator.crunchydata.com/v1beta1"

type Owner struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type Metadata struct {
	Namespace         string            `json:"namespace"`
	Name              string            `json:"name"`
	UID               string            `json:"uid"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Owners            []Owner           `json:"ownerReferences"`
	// Generation and Annotations are read by checks other than pgBackRest.
	Generation  *int64            `json:"generation,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Backup struct {
	Repo           string `json:"repo"`
	Succeeded      int    `json:"succeeded"`
	CompletionTime string `json:"completionTime"`
	StartTime      string `json:"startTime"`
}

// Repository contains only destination settings; schedules do not change identity.
type Repository struct {
	Name   string            `json:"name"`
	S3     map[string]string `json:"s3,omitempty"`
	GCS    map[string]string `json:"gcs,omitempty"`
	Azure  map[string]string `json:"azure,omitempty"`
	Volume *struct {
		VolumeClaimSpec struct {
			StorageClassName *string        `json:"storageClassName,omitempty"`
			VolumeName       string         `json:"volumeName,omitempty"`
			Selector         map[string]any `json:"selector,omitempty"`
			DataSource       map[string]any `json:"dataSource,omitempty"`
			DataSourceRef    map[string]any `json:"dataSourceRef,omitempty"`
		} `json:"volumeClaimSpec"`
	} `json:"volume,omitempty"`
}

type Cluster struct {
	Metadata Metadata `json:"metadata"`
	Spec     struct {
		Backups struct {
			PGBackRest struct {
				Repos  []Repository      `json:"repos"`
				Global map[string]string `json:"global"`
			} `json:"pgbackrest"`
		} `json:"backups"`
	} `json:"spec"`
	Status struct {
		PGBackRest struct {
			ScheduledBackups []Backup `json:"scheduledBackups"`
			Repos            []struct {
				Name          string `json:"name"`
				Volume        string `json:"volume"`
				StanzaCreated bool   `json:"stanzaCreated"`
			} `json:"repos"`
		} `json:"pgbackrest"`
	} `json:"status"`
}

type Job struct {
	Metadata Metadata `json:"metadata"`
	Status   struct {
		Succeeded      int    `json:"succeeded"`
		CompletionTime string `json:"completionTime"`
		Conditions     []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type CronJob struct {
	Metadata Metadata `json:"metadata"`
	Status   struct {
		LastSuccessfulTime string `json:"lastSuccessfulTime"`
	} `json:"status"`
}
