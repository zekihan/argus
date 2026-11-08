// Package config loads Argus's strict JSON configuration.
//
// Unknown fields are rejected. Every field added after a chart release must be
// optional with a safe default, so an edge image accepts the configuration the
// latest released chart renders.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const maxSeconds = 366 * 24 * 3600

// Schedule overrides the global collection timing for one check.
type Schedule struct {
	IntervalSeconds          int `json:"intervalSeconds,omitempty"`
	CollectionTimeoutSeconds int `json:"collectionTimeoutSeconds,omitempty"`
	MaxSampleAgeSeconds      int `json:"maxSampleAgeSeconds,omitempty"`
}

type TLS struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

type State struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type NamespacedName struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (n NamespacedName) Key() string { return n.Namespace + "/" + n.Name }

type Repository struct {
	MaxAgeSeconds int `json:"maxAgeSeconds,omitempty"`
}

type Cluster struct {
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Repos     map[string]Repository `json:"repos"`
}

type PGBackRest struct {
	Schedule
	MaxAgeSeconds int       `json:"maxAgeSeconds,omitempty"`
	Clusters      []Cluster `json:"clusters"`
}

type Postgres struct {
	Schedule
	Clusters []NamespacedName `json:"clusters"`
}

type Longhorn struct {
	Schedule
	Namespace                 string   `json:"namespace"`
	ExpectedPVCs              []string `json:"expectedPVCs"`
	BackupGroup               string   `json:"backupGroup,omitempty"`
	MaxBackupAgeSeconds       int      `json:"maxBackupAgeSeconds"`
	SystemBackupMaxAgeSeconds int      `json:"systemBackupMaxAgeSeconds"`
	SystemBackupGraceSeconds  int      `json:"systemBackupGraceSeconds,omitempty"`
}

type CronJob struct {
	Namespace              string `json:"namespace"`
	Name                   string `json:"name"`
	MaxAgeSeconds          int    `json:"maxAgeSeconds"`
	MaxRunSeconds          int    `json:"maxRunSeconds,omitempty"`
	MaxConsecutiveFailures int    `json:"maxConsecutiveFailures,omitempty"`
}

func (c CronJob) Key() string { return c.Namespace + "/" + c.Name }

type CronJobs struct {
	Schedule
	Items []CronJob `json:"items"`
}

type Certificates struct {
	Schedule
	Expected              []NamespacedName `json:"expected,omitempty"`
	ExcludeIssuers        []string         `json:"excludeIssuers,omitempty"`
	NotReadySeconds       int              `json:"notReadySeconds,omitempty"`
	RenewalOverdueSeconds int              `json:"renewalOverdueSeconds,omitempty"`
	ExpiryDays            int              `json:"expiryDays,omitempty"`
}

type PKI struct {
	Schedule
	RootNamespace        string           `json:"rootNamespace,omitempty"`
	RootName             string           `json:"rootName,omitempty"`
	IssuerName           string           `json:"issuerName,omitempty"`
	RootSHA256           string           `json:"rootSHA256"`
	RootSPKI             string           `json:"rootSPKI,omitempty"`
	ExpectedCertificates []NamespacedName `json:"expectedCertificates"`
	ExpectedBundles      []string         `json:"expectedBundles,omitempty"`
	LeafDays             int              `json:"leafDays,omitempty"`
	MarginDays           int              `json:"marginDays,omitempty"`
}

type ExternalSecrets struct {
	Schedule
	ExpectedExternalSecrets        []NamespacedName `json:"expectedExternalSecrets,omitempty"`
	ExpectedClusterExternalSecrets []string         `json:"expectedClusterExternalSecrets,omitempty"`
	ExpectedSecretStores           []NamespacedName `json:"expectedSecretStores,omitempty"`
	ExpectedClusterSecretStores    []string         `json:"expectedClusterSecretStores,omitempty"`
	RefreshGraceSeconds            int              `json:"refreshGraceSeconds,omitempty"`
}

type ArgoCD struct {
	Schedule
	Namespace                 string   `json:"namespace"`
	Expected                  []string `json:"expected,omitempty"`
	AutomatedOutOfSyncSeconds int      `json:"automatedOutOfSyncSeconds,omitempty"`
	ManualOutOfSyncSeconds    int      `json:"manualOutOfSyncSeconds,omitempty"`
	ProgressingSeconds        int      `json:"progressingSeconds,omitempty"`
}

type Nodes struct {
	Schedule
	Expected                    []string `json:"expected"`
	PersistSeconds              int      `json:"persistSeconds,omitempty"`
	Metrics                     *bool    `json:"metrics,omitempty"`
	CPUPercent                  int      `json:"cpuPercent,omitempty"`
	MemoryPercent               int      `json:"memoryPercent,omitempty"`
	LonghornNamespace           string   `json:"longhornNamespace,omitempty"`
	LonghornMinAvailablePercent int      `json:"longhornMinAvailablePercent,omitempty"`
}

// MetricsEnabled reports whether metrics.k8s.io usage is evaluated (default true).
func (n Nodes) MetricsEnabled() bool { return n.Metrics == nil || *n.Metrics }

type Host struct {
	Name    string   `json:"name"`
	Address string   `json:"address"`
	Expects []string `json:"expects,omitempty"`
}

type Hosts struct {
	Schedule
	User           string `json:"user,omitempty"`
	PrivateKeyFile string `json:"privateKeyFile"`
	KnownHostsFile string `json:"knownHostsFile"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	Concurrency    int    `json:"concurrency,omitempty"`
	Hosts          []Host `json:"hosts"`
}

// Checks holds one optional section per check. An absent section disables it.
type Checks struct {
	Hosts           *Hosts           `json:"hosts,omitempty"`
	Nodes           *Nodes           `json:"nodes,omitempty"`
	Postgres        *Postgres        `json:"postgres,omitempty"`
	PGBackRest      *PGBackRest      `json:"pgbackrest,omitempty"`
	Longhorn        *Longhorn        `json:"longhorn,omitempty"`
	CronJobs        *CronJobs        `json:"cronjobs,omitempty"`
	Certificates    *Certificates    `json:"certificates,omitempty"`
	PKI             *PKI             `json:"pki,omitempty"`
	ExternalSecrets *ExternalSecrets `json:"externalsecrets,omitempty"`
	ArgoCD          *ArgoCD          `json:"argocd,omitempty"`
}

type Config struct {
	ListenAddress            string `json:"listenAddress"`
	TokenFile                string `json:"tokenFile"`
	TLS                      *TLS   `json:"tls,omitempty"`
	State                    State  `json:"state"`
	PollIntervalSeconds      int    `json:"pollIntervalSeconds"`
	MaxSampleAgeSeconds      int    `json:"maxSampleAgeSeconds"`
	RequestTimeoutSeconds    int    `json:"requestTimeoutSeconds"`
	CollectionTimeoutSeconds int    `json:"collectionTimeoutSeconds"`
	Checks                   Checks `json:"checks"`
}

func Default() Config {
	return Config{ListenAddress: ":8443", TokenFile: "/var/run/argus-auth/token",
		State:               State{Namespace: "argus", Name: "argus-state"},
		PollIntervalSeconds: 60, MaxSampleAgeSeconds: 180, RequestTimeoutSeconds: 10, CollectionTimeoutSeconds: 45}
}

// Load reads, strictly decodes, defaults and validates a configuration file.
func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("open configuration: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return c, fmt.Errorf("configuration exceeds read budget or cannot be read")
	}
	return Parse(data)
}

// Parse strictly decodes, defaults and validates configuration JSON.
func Parse(data []byte) (Config, error) {
	c := Default()
	if err := UniqueJSON(data); err != nil {
		return c, fmt.Errorf("invalid configuration JSON: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	c.ApplyDefaults()
	return c, c.Validate()
}

// ApplyDefaults fills optional fields with their documented defaults.
func (c *Config) ApplyDefaults() {
	k := &c.Checks
	if p := k.PGBackRest; p != nil && p.MaxAgeSeconds == 0 {
		p.MaxAgeSeconds = 28800
	}
	if l := k.Longhorn; l != nil && l.BackupGroup == "" {
		l.BackupGroup = "backup"
	}
	if j := k.CronJobs; j != nil {
		for i := range j.Items {
			if j.Items[i].MaxRunSeconds == 0 {
				j.Items[i].MaxRunSeconds = 3600
			}
			if j.Items[i].MaxConsecutiveFailures == 0 {
				j.Items[i].MaxConsecutiveFailures = 2
			}
		}
	}
	if x := k.Certificates; x != nil {
		x.NotReadySeconds = orDefault(x.NotReadySeconds, 3600)
		x.RenewalOverdueSeconds = orDefault(x.RenewalOverdueSeconds, 86400)
		x.ExpiryDays = orDefault(x.ExpiryDays, 14)
	}
	if p := k.PKI; p != nil {
		p.RootNamespace = orDefaultString(p.RootNamespace, "cert-manager")
		p.RootName = orDefaultString(p.RootName, "selfsigned-ca")
		p.IssuerName = orDefaultString(p.IssuerName, "selfsigned-ca-issuer")
		p.LeafDays = orDefault(p.LeafDays, 90)
		p.MarginDays = orDefault(p.MarginDays, 90)
	}
	if e := k.ExternalSecrets; e != nil {
		e.RefreshGraceSeconds = orDefault(e.RefreshGraceSeconds, 600)
	}
	if a := k.ArgoCD; a != nil {
		a.AutomatedOutOfSyncSeconds = orDefault(a.AutomatedOutOfSyncSeconds, 900)
		a.ManualOutOfSyncSeconds = orDefault(a.ManualOutOfSyncSeconds, 7*24*3600)
		a.ProgressingSeconds = orDefault(a.ProgressingSeconds, 900)
	}
	if n := k.Nodes; n != nil {
		n.PersistSeconds = orDefault(n.PersistSeconds, 7200)
		n.CPUPercent = orDefault(n.CPUPercent, 90)
		n.MemoryPercent = orDefault(n.MemoryPercent, 90)
		n.LonghornMinAvailablePercent = orDefault(n.LonghornMinAvailablePercent, 10)
	}
	if h := k.Hosts; h != nil {
		h.User = orDefaultString(h.User, "argus")
		h.TimeoutSeconds = orDefault(h.TimeoutSeconds, 30)
		h.Concurrency = orDefault(h.Concurrency, 8)
	}
}

func orDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func orDefaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
var repoName = regexp.MustCompile(`^repo[1-4]$`)
var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Name reports whether s is a Kubernetes DNS label (namespace and most object names).
func Name(s string) bool { return len(s) <= 63 && dnsLabel.MatchString(s) }

// Object reports whether s is a Kubernetes DNS subdomain object name.
func Object(s string) bool { return len(s) <= 253 && dnsSubdomain.MatchString(s) }

// Repo reports whether s is a pgBackRest repository name.
func Repo(s string) bool { return repoName.MatchString(s) }

// HostDimensions are the probe fields a host may declare in expects.
var HostDimensions = []string{"disk", "inodes", "memory", "swap", "load", "cpu_pressure", "uptime", "failed_units", "zfs", "time_sync", "ups"}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.TokenFile) || filepath.Base(c.TokenFile) == "." {
		return fmt.Errorf("tokenFile must be an absolute file path")
	}
	_, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fmt.Errorf("listenAddress must be host:port with a valid port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || strings.Trim(port, "0123456789") != "" || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("listenAddress port must be between 1 and 65535")
	}
	if c.TLS != nil && (!absFile(c.TLS.CertFile) || !absFile(c.TLS.KeyFile)) {
		return fmt.Errorf("tls.certFile and tls.keyFile must be absolute file paths")
	}
	if !Name(c.State.Namespace) || !Object(c.State.Name) {
		return fmt.Errorf("state must name a ConfigMap namespace and name")
	}
	if err := timing("", c.PollIntervalSeconds, c.CollectionTimeoutSeconds, c.MaxSampleAgeSeconds, c.RequestTimeoutSeconds); err != nil {
		return err
	}
	for _, check := range c.Enabled() {
		interval, timeout, maxAge := c.Timing(check.Schedule)
		if err := timing(check.Name+".", interval, timeout, maxAge, c.RequestTimeoutSeconds); err != nil {
			return err
		}
		if err := check.validate(); err != nil {
			return fmt.Errorf("checks.%s: %w", check.Name, err)
		}
		if h := c.Checks.Hosts; check.Name == "hosts" {
			// Hosts are probed in batches of concurrency, each bounded by
			// timeoutSeconds; every batch must fit the collection timeout, or
			// healthy hosts late in the queue would report timeouts.
			batches := (len(h.Hosts) + h.Concurrency - 1) / h.Concurrency
			if batches*h.TimeoutSeconds > timeout {
				return fmt.Errorf("checks.hosts: %d batches of %d hosts at timeoutSeconds %d exceed collectionTimeoutSeconds %d", batches, h.Concurrency, h.TimeoutSeconds, timeout)
			}
		}
	}
	return nil
}

func timing(prefix string, interval, timeout, maxAge, request int) error {
	for name, value := range map[string]int{"pollIntervalSeconds": interval, "maxSampleAgeSeconds": maxAge,
		"requestTimeoutSeconds": request, "collectionTimeoutSeconds": timeout} {
		if value < 1 || value > maxSeconds {
			return fmt.Errorf("%s%s must be between 1 and 31622400", prefix, name)
		}
	}
	if request > timeout || interval+2*timeout >= maxAge {
		return fmt.Errorf("%srequest timeout must fit collection timeout; poll interval plus two collection timeouts must be shorter than sample lifetime", prefix)
	}
	return nil
}

// Timing resolves a check's effective interval, collection timeout and maximum sample age.
func (c Config) Timing(s Schedule) (interval, timeout, maxAge int) {
	interval, timeout, maxAge = c.PollIntervalSeconds, c.CollectionTimeoutSeconds, c.MaxSampleAgeSeconds
	if s.IntervalSeconds != 0 {
		interval = s.IntervalSeconds
	}
	if s.CollectionTimeoutSeconds != 0 {
		timeout = s.CollectionTimeoutSeconds
	}
	if s.MaxSampleAgeSeconds != 0 {
		maxAge = s.MaxSampleAgeSeconds
	}
	return interval, timeout, maxAge
}

// EnabledCheck names an enabled check section and its schedule.
type EnabledCheck struct {
	Name     string
	Schedule Schedule
	validate func() error
}

// Enabled lists the configured checks in a stable order.
func (c Config) Enabled() []EnabledCheck {
	k := c.Checks
	var out []EnabledCheck
	add := func(present bool, name string, s func() Schedule, v func() error) {
		if present {
			out = append(out, EnabledCheck{Name: name, Schedule: s(), validate: v})
		}
	}
	add(k.Hosts != nil, "hosts", func() Schedule { return k.Hosts.Schedule }, func() error { return k.Hosts.validate() })
	add(k.Nodes != nil, "nodes", func() Schedule { return k.Nodes.Schedule }, func() error { return k.Nodes.validate() })
	add(k.Postgres != nil, "postgres", func() Schedule { return k.Postgres.Schedule }, func() error { return k.Postgres.validate() })
	add(k.PGBackRest != nil, "pgbackrest", func() Schedule { return k.PGBackRest.Schedule }, func() error { return k.PGBackRest.validate() })
	add(k.Longhorn != nil, "longhorn", func() Schedule { return k.Longhorn.Schedule }, func() error { return k.Longhorn.validate() })
	add(k.CronJobs != nil, "cronjobs", func() Schedule { return k.CronJobs.Schedule }, func() error { return k.CronJobs.validate() })
	add(k.Certificates != nil, "certificates", func() Schedule { return k.Certificates.Schedule }, func() error { return k.Certificates.validate() })
	add(k.PKI != nil, "pki", func() Schedule { return k.PKI.Schedule }, func() error { return k.PKI.validate() })
	add(k.ExternalSecrets != nil, "externalsecrets", func() Schedule { return k.ExternalSecrets.Schedule }, func() error { return k.ExternalSecrets.validate() })
	add(k.ArgoCD != nil, "argocd", func() Schedule { return k.ArgoCD.Schedule }, func() error { return k.ArgoCD.validate() })
	return out
}

func absFile(path string) bool {
	return filepath.IsAbs(path) && filepath.Base(path) != "." && filepath.Base(path) != "/"
}

func seconds(name string, value int, required bool) error {
	if (required && value < 1) || value < 0 || value > maxSeconds {
		return fmt.Errorf("%s must be between 1 and 31622400", name)
	}
	return nil
}

func uniqueNamespaced(field string, items []NamespacedName, required bool) error {
	if required && len(items) == 0 {
		return fmt.Errorf("%s must list an explicit inventory", field)
	}
	if len(items) > 5000 {
		return fmt.Errorf("%s exceeds 5000 entries", field)
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !Name(item.Namespace) || !Object(item.Name) || seen[item.Key()] {
			return fmt.Errorf("invalid or duplicate %s entry: %s", field, item.Key())
		}
		seen[item.Key()] = true
	}
	return nil
}

func uniqueNames(field string, items []string, required bool, valid func(string) bool) error {
	if required && len(items) == 0 {
		return fmt.Errorf("%s must list an explicit inventory", field)
	}
	if len(items) > 5000 {
		return fmt.Errorf("%s exceeds 5000 entries", field)
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !valid(item) || seen[item] {
			return fmt.Errorf("invalid or duplicate %s entry: %s", field, item)
		}
		seen[item] = true
	}
	return nil
}

func (p *PGBackRest) validate() error {
	if err := seconds("maxAgeSeconds", p.MaxAgeSeconds, true); err != nil {
		return err
	}
	if len(p.Clusters) == 0 || len(p.Clusters) > 1000 {
		return fmt.Errorf("clusters must contain an explicit inventory of 1 to 1000 clusters")
	}
	seen := map[string]bool{}
	for _, cluster := range p.Clusters {
		key := cluster.Namespace + "/" + cluster.Name
		if !Name(cluster.Namespace) || !Name(cluster.Name) || seen[key] || len(cluster.Repos) == 0 {
			return fmt.Errorf("invalid or duplicate cluster inventory: %s", key)
		}
		seen[key] = true
		for repo, override := range cluster.Repos {
			if !Repo(repo) || override.MaxAgeSeconds < 0 || override.MaxAgeSeconds > maxSeconds {
				return fmt.Errorf("invalid repository or freshness override: %s/%s", key, repo)
			}
		}
	}
	return nil
}

func (p *Postgres) validate() error {
	if len(p.Clusters) == 0 {
		return fmt.Errorf("clusters must list an explicit inventory")
	}
	for _, cluster := range p.Clusters {
		if !Name(cluster.Name) {
			return fmt.Errorf("invalid cluster name: %s", cluster.Key())
		}
	}
	return uniqueNamespaced("clusters", p.Clusters, true)
}

func (l *Longhorn) validate() error {
	if !Name(l.Namespace) {
		return fmt.Errorf("namespace must be the Longhorn namespace")
	}
	if !Name(l.BackupGroup) {
		return fmt.Errorf("backupGroup must be a recurring job group name")
	}
	if err := uniqueNames("expectedPVCs", l.ExpectedPVCs, true, func(s string) bool {
		namespace, name, ok := strings.Cut(s, "/")
		return ok && Name(namespace) && Object(name)
	}); err != nil {
		return err
	}
	for name, value := range map[string]int{"maxBackupAgeSeconds": l.MaxBackupAgeSeconds, "systemBackupMaxAgeSeconds": l.SystemBackupMaxAgeSeconds} {
		if err := seconds(name, value, true); err != nil {
			return err
		}
	}
	return seconds("systemBackupGraceSeconds", l.SystemBackupGraceSeconds, false)
}

func (j *CronJobs) validate() error {
	if len(j.Items) == 0 || len(j.Items) > 5000 {
		return fmt.Errorf("items must list an explicit inventory of CronJobs")
	}
	seen := map[string]bool{}
	for _, item := range j.Items {
		if !Name(item.Namespace) || !Name(item.Name) || len(item.Name) > 52 || seen[item.Key()] {
			return fmt.Errorf("invalid or duplicate CronJob: %s", item.Key())
		}
		seen[item.Key()] = true
		if err := seconds(item.Key()+" maxAgeSeconds", item.MaxAgeSeconds, true); err != nil {
			return err
		}
		if err := seconds(item.Key()+" maxRunSeconds", item.MaxRunSeconds, true); err != nil {
			return err
		}
		if item.MaxConsecutiveFailures < 1 || item.MaxConsecutiveFailures > 1000 {
			return fmt.Errorf("%s maxConsecutiveFailures must be between 1 and 1000", item.Key())
		}
	}
	return nil
}

func (x *Certificates) validate() error {
	if err := uniqueNamespaced("expected", x.Expected, false); err != nil {
		return err
	}
	if err := uniqueNames("excludeIssuers", x.ExcludeIssuers, false, Object); err != nil {
		return err
	}
	for name, value := range map[string]int{"notReadySeconds": x.NotReadySeconds, "renewalOverdueSeconds": x.RenewalOverdueSeconds} {
		if err := seconds(name, value, true); err != nil {
			return err
		}
	}
	if x.ExpiryDays < 1 || x.ExpiryDays > 3650 {
		return fmt.Errorf("expiryDays must be between 1 and 3650")
	}
	return nil
}

func (p *PKI) validate() error {
	if !Name(p.RootNamespace) || !Object(p.RootName) || !Object(p.IssuerName) {
		return fmt.Errorf("rootNamespace, rootName and issuerName must be Kubernetes names")
	}
	if !hex64.MatchString(p.RootSHA256) || (p.RootSPKI != "" && !hex64.MatchString(p.RootSPKI)) {
		return fmt.Errorf("rootSHA256 and rootSPKI must be lowercase hex SHA-256 digests")
	}
	if err := uniqueNamespaced("expectedCertificates", p.ExpectedCertificates, true); err != nil {
		return err
	}
	if err := uniqueNames("expectedBundles", p.ExpectedBundles, false, Object); err != nil {
		return err
	}
	if p.LeafDays < 1 || p.LeafDays > 3650 || p.MarginDays < 1 || p.MarginDays > 3650 {
		return fmt.Errorf("leafDays and marginDays must be between 1 and 3650")
	}
	return nil
}

func (e *ExternalSecrets) validate() error {
	if err := uniqueNamespaced("expectedExternalSecrets", e.ExpectedExternalSecrets, false); err != nil {
		return err
	}
	if err := uniqueNamespaced("expectedSecretStores", e.ExpectedSecretStores, false); err != nil {
		return err
	}
	if err := uniqueNames("expectedClusterExternalSecrets", e.ExpectedClusterExternalSecrets, false, Object); err != nil {
		return err
	}
	if err := uniqueNames("expectedClusterSecretStores", e.ExpectedClusterSecretStores, false, Object); err != nil {
		return err
	}
	return seconds("refreshGraceSeconds", e.RefreshGraceSeconds, true)
}

func (a *ArgoCD) validate() error {
	if !Name(a.Namespace) {
		return fmt.Errorf("namespace must be the Argo CD namespace")
	}
	if err := uniqueNames("expected", a.Expected, false, Object); err != nil {
		return err
	}
	for name, value := range map[string]int{"automatedOutOfSyncSeconds": a.AutomatedOutOfSyncSeconds,
		"manualOutOfSyncSeconds": a.ManualOutOfSyncSeconds, "progressingSeconds": a.ProgressingSeconds} {
		if err := seconds(name, value, true); err != nil {
			return err
		}
	}
	return nil
}

func (n *Nodes) validate() error {
	if err := uniqueNames("expected", n.Expected, true, Object); err != nil {
		return err
	}
	if n.LonghornNamespace != "" && !Name(n.LonghornNamespace) {
		return fmt.Errorf("longhornNamespace must be a namespace name")
	}
	for name, value := range map[string]int{"cpuPercent": n.CPUPercent, "memoryPercent": n.MemoryPercent, "longhornMinAvailablePercent": n.LonghornMinAvailablePercent} {
		if value < 1 || value > 100 {
			return fmt.Errorf("%s must be between 1 and 100", name)
		}
	}
	return seconds("persistSeconds", n.PersistSeconds, true)
}

var hostName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func (h *Hosts) validate() error {
	if !Name(h.User) {
		return fmt.Errorf("user must be a simple login name")
	}
	if !absFile(h.PrivateKeyFile) || !absFile(h.KnownHostsFile) {
		return fmt.Errorf("privateKeyFile and knownHostsFile must be absolute file paths")
	}
	if h.TimeoutSeconds < 1 || h.TimeoutSeconds > 600 || h.Concurrency < 1 || h.Concurrency > 64 {
		return fmt.Errorf("timeoutSeconds must be 1-600 and concurrency 1-64")
	}
	if len(h.Hosts) == 0 || len(h.Hosts) > 500 {
		return fmt.Errorf("hosts must list an explicit inventory of 1 to 500 hosts")
	}
	seen := map[string]bool{}
	for _, host := range h.Hosts {
		if len(host.Name) > 63 || !hostName.MatchString(host.Name) || seen[host.Name] {
			return fmt.Errorf("invalid or duplicate host name: %s", host.Name)
		}
		seen[host.Name] = true
		if _, err := HostAddress(host.Address); err != nil {
			return fmt.Errorf("host %s: %w", host.Name, err)
		}
		if err := uniqueNames("expects", host.Expects, false, func(s string) bool {
			for _, d := range HostDimensions {
				if d == s {
					return true
				}
			}
			return false
		}); err != nil {
			return fmt.Errorf("host %s: %w", host.Name, err)
		}
	}
	return nil
}

// HostAddress normalises host or host:port to host:port, defaulting to port 22.
func HostAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host, port = address, "22"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strings.Trim(port, "0123456789") != "" {
		return "", fmt.Errorf("address port must be between 1 and 65535")
	}
	if net.ParseIP(host) == nil && (len(host) > 253 || !dnsSubdomain.MatchString(host)) {
		return "", fmt.Errorf("address must be an IP address or DNS name")
	}
	return net.JoinHostPort(host, port), nil
}

// UniqueJSON rejects case-insensitive duplicate keys and trailing JSON documents.
// Key comparison matches encoding/json's case-insensitive struct field matching.
func UniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					name = foldKey(name)
					if !ok || seen[name] {
						return fmt.Errorf("duplicate or invalid object key")
					}
					seen[name] = true
					if err := walk(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := walk(); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("unexpected delimiter")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON content")
	}
	return nil
}

func foldKey(key string) string {
	return strings.Map(func(r rune) rune {
		// Choose one representative for the entire Unicode case-folding cycle,
		// including aliases such as the long s that simple lowercasing misses.
		folded := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded {
				folded = next
			}
		}
		return folded
	}, key)
}
