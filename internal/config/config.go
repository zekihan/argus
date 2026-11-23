// Package config loads Argus's strict JSON configuration.
//
// Unknown fields are rejected. Every field added after a chart release must be
// optional with a safe default, so an edge image accepts the configuration the
// latest released chart renders.
package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
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

type PVCs struct {
	Schedule
	StorageClasses     []string         `json:"storageClasses"`
	ExpectedNodes      []string         `json:"expectedNodes"`
	Expected           []NamespacedName `json:"expected,omitempty"`
	AllowUnmounted     []NamespacedName `json:"allowUnmounted,omitempty"`
	UsedPercent        int              `json:"usedPercent,omitempty"`
	MaxStatsAgeSeconds int              `json:"maxStatsAgeSeconds,omitempty"`
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

// defaultPBSMaxAgeSeconds is 26h: a nightly vzdump may finish after midnight.
const defaultPBSMaxAgeSeconds = 26 * 3600

// PBSMaxAgeOverride replaces maxAgeSeconds for backup groups whose key matches
// Match. Match is store/type/id; * and ? apply inside one segment and do not
// cross a slash. The first listed override is not special: overlapping matches
// are rejected.
type PBSMaxAgeOverride struct {
	Match         string `json:"match"`
	MaxAgeSeconds int    `json:"maxAgeSeconds"`
}

// PBS is the Proxmox Backup Server backup-group check.
type PBS struct {
	Schedule
	URL             string              `json:"url"`
	TokenFile       string              `json:"tokenFile"`
	CAFile          string              `json:"caFile,omitempty"`
	Fingerprint     string              `json:"fingerprint,omitempty"`
	MaxAgeSeconds   int                 `json:"maxAgeSeconds,omitempty"`
	MaxAgeOverrides []PBSMaxAgeOverride `json:"maxAgeOverrides,omitempty"`
	Expected        []string            `json:"expected,omitempty"`
	Exclude         []string            `json:"exclude,omitempty"`
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

var hostLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

type DoH struct {
	Schedule
	URL          string `json:"url"`
	Name         string `json:"name"`
	ExpectedIPv4 string `json:"expectedIPv4"`
	DNSResolver  string `json:"dnsResolver,omitempty"`
}

func (d *DoH) validate() error {
	u, err := url.Parse(d.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("url must be an HTTPS URL without credentials, query or fragment")
	}
	if d.DNSResolver != "" {
		host, port, err := net.SplitHostPort(d.DNSResolver)
		if err != nil || net.ParseIP(host) == nil || port != "53" {
			return fmt.Errorf("dnsResolver must be an IP address on port 53")
		}
	}
	name := strings.TrimSuffix(d.Name, ".")
	if len(name) == 0 || len(name) > 253 {
		return fmt.Errorf("name must be a DNS hostname")
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 || !hostLabel.MatchString(label) {
			return fmt.Errorf("name must be a DNS hostname")
		}
	}
	if ip := net.ParseIP(d.ExpectedIPv4); ip == nil || ip.To4() == nil {
		return fmt.Errorf("expectedIPv4 must be an IPv4 address")
	}
	return nil
}

// Checks holds one optional section per check. An absent section disables it.
type Checks struct {
	DoH             *DoH             `json:"doh,omitempty"`
	PVCs            *PVCs            `json:"pvcs,omitempty"`
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
	PBS             *PBS             `json:"pbs,omitempty"`
}

type Config struct {
	ListenAddress            string `json:"listenAddress"`
	MetricsAddress           string `json:"metricsAddress,omitempty"`
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
	return Config{ListenAddress: ":8443", MetricsAddress: ":9090", TokenFile: "/var/run/argus-auth/token",
		State:               State{Namespace: "argus", Name: "argus-state"},
		PollIntervalSeconds: 60, MaxSampleAgeSeconds: 180, RequestTimeoutSeconds: 10, CollectionTimeoutSeconds: 45}
}

// Load reads, strictly decodes, defaults and validates a configuration file.
func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return c, FileError("open configuration", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return c, fmt.Errorf("configuration exceeds read budget or cannot be read")
	}
	return Parse(data)
}

// Parse strictly decodes, defaults and validates a JSON configuration object.
func Parse(data []byte) (Config, error) {
	c := Default()
	if err := UniqueJSON(data); err != nil {
		return c, fmt.Errorf("invalid configuration JSON: %s", JSONProblem(err))
	}
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return c, fmt.Errorf("configuration must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %s", JSONProblem(err))
	}
	c.ApplyDefaults()
	return c, c.Validate()
}

// ApplyDefaults fills optional fields with their documented defaults.
func (c *Config) ApplyDefaults() {
	k := &c.Checks
	if p := k.PVCs; p != nil {
		p.UsedPercent = orDefault(p.UsedPercent, 80)
		p.MaxStatsAgeSeconds = orDefault(p.MaxStatsAgeSeconds, 300)
	}
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
	if p := k.PBS; p != nil && p.MaxAgeSeconds == 0 {
		p.MaxAgeSeconds = defaultPBSMaxAgeSeconds
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
	if c.MetricsAddress != "" {
		if _, port, err := net.SplitHostPort(c.MetricsAddress); err != nil {
			return fmt.Errorf("metricsAddress must be host:port with a valid port")
		} else if portNumber, err := strconv.Atoi(port); err != nil || strings.Trim(port, "0123456789") != "" || portNumber < 1 || portNumber > 65535 {
			return fmt.Errorf("metricsAddress port must be between 1 and 65535")
		}
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
	add(k.DoH != nil, "doh", func() Schedule { return k.DoH.Schedule }, func() error { return k.DoH.validate() })
	add(k.PVCs != nil, "pvcs", func() Schedule { return k.PVCs.Schedule }, func() error { return k.PVCs.validate() })
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
	add(k.PBS != nil, "pbs", func() Schedule { return k.PBS.Schedule }, func() error { return k.PBS.validate() })
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
var pbsName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,63})$`)

// PBSName reports whether s is one PBS datastore, backup type, backup id or comment segment.
func PBSName(s string) bool { return pbsName.MatchString(s) }

func pbsGroup(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !PBSName(part) {
			return false
		}
	}
	return true
}

func (p *PBS) validate() error {
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("url must be an HTTPS origin without credentials, query or fragment")
	}
	if !absFile(p.TokenFile) {
		return fmt.Errorf("tokenFile must be an absolute file path")
	}
	if p.CAFile != "" && !absFile(p.CAFile) {
		return fmt.Errorf("caFile must be an absolute file path")
	}
	if p.Fingerprint != "" && !sha256Fingerprint(p.Fingerprint) {
		return fmt.Errorf("fingerprint must be a SHA-256 digest")
	}
	if p.CAFile == "" && p.Fingerprint == "" {
		return fmt.Errorf("caFile or fingerprint must pin the PBS certificate")
	}
	if err := seconds("maxAgeSeconds", p.MaxAgeSeconds, true); err != nil {
		return err
	}
	if err := p.validateMaxAgeOverrides(); err != nil {
		return err
	}
	if err := uniqueNames("expected", p.Expected, false, pbsGroup); err != nil {
		return err
	}
	if err := uniqueNames("exclude", p.Exclude, false, pbsGroup); err != nil {
		return err
	}
	skip := map[string]bool{}
	for _, key := range p.Exclude {
		skip[key] = true
	}
	for _, key := range p.Expected {
		if skip[key] {
			return fmt.Errorf("expected and exclude both contain %s", key)
		}
	}
	return nil
}

func (p *PBS) validateMaxAgeOverrides() error {
	if len(p.MaxAgeOverrides) > 5000 {
		return fmt.Errorf("maxAgeOverrides exceeds 5000 entries")
	}
	seen := map[string]bool{}
	for i, override := range p.MaxAgeOverrides {
		if !pbsGlob(override.Match) || seen[override.Match] {
			return fmt.Errorf("invalid or duplicate maxAgeOverrides match: %s", override.Match)
		}
		seen[override.Match] = true
		if err := seconds(fmt.Sprintf("maxAgeOverrides[%d].maxAgeSeconds", i), override.MaxAgeSeconds, true); err != nil {
			return err
		}
		for _, earlier := range p.MaxAgeOverrides[:i] {
			if pbsGlobsOverlap(earlier.Match, override.Match) {
				return fmt.Errorf("maxAgeOverrides match %s overlaps %s", earlier.Match, override.Match)
			}
		}
	}
	return nil
}

// MaxAge returns the freshness limit for a backup group key. The second result
// is false when more than one override matches.
func (p PBS) MaxAge(group string) (int, bool) {
	matched := 0
	seconds := p.MaxAgeSeconds
	for _, override := range p.MaxAgeOverrides {
		ok, err := filepath.Match(override.Match, group)
		if err != nil || !ok {
			continue
		}
		matched++
		seconds = override.MaxAgeSeconds
	}
	if matched > 1 {
		return 0, false
	}
	return seconds, true
}

var pbsGlobSegment = regexp.MustCompile(`^[A-Za-z0-9_.*?-]+$`)

func pbsGlob(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !pbsGlobSegment.MatchString(part) {
			return false
		}
		if _, err := filepath.Match(part, "a"); err != nil {
			return false
		}
	}
	return true
}

// pbsGlobsOverlap reports whether any store/type/id key can match both globs.
func pbsGlobsOverlap(a, b string) bool {
	left, right := strings.Split(a, "/"), strings.Split(b, "/")
	if len(left) != 3 || len(right) != 3 {
		return false
	}
	for i := range left {
		if !segmentGlobsOverlap(left[i], right[i]) {
			return false
		}
	}
	return true
}

// segmentGlobsOverlap reports whether two single-segment globs (* and ?) match
// a common string. The product of their NFAs accepts when they do.
func segmentGlobsOverlap(a, b string) bool {
	literals := map[byte]int{}
	for _, pattern := range []string{a, b} {
		for i := range len(pattern) {
			c := pattern[i]
			if c != '*' && c != '?' {
				if _, ok := literals[c]; !ok {
					literals[c] = len(literals)
				}
			}
		}
	}
	other := len(literals)
	symbols := other + 1
	symbol := func(c byte) int {
		if id, ok := literals[c]; ok {
			return id
		}
		return other
	}
	type machine struct {
		eps    [][]int
		trans  [][][]int
		accept int
	}
	build := func(pattern string) machine {
		n := len(pattern) + 1
		m := machine{eps: make([][]int, n), trans: make([][][]int, n), accept: len(pattern)}
		for i := range m.trans {
			m.trans[i] = make([][]int, symbols)
		}
		for i := range len(pattern) {
			switch pattern[i] {
			case '*':
				m.eps[i] = append(m.eps[i], i+1)
				for s := range symbols {
					m.trans[i][s] = append(m.trans[i][s], i)
				}
			case '?':
				for s := range symbols {
					m.trans[i][s] = append(m.trans[i][s], i+1)
				}
			default:
				s := symbol(pattern[i])
				m.trans[i][s] = append(m.trans[i][s], i+1)
			}
		}
		return m
	}
	left, right := build(a), build(b)
	type pair struct{ i, j int }
	close := func(start pair) []pair {
		seen := map[pair]bool{start: true}
		queue := []pair{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, i := range left.eps[cur.i] {
				next := pair{i, cur.j}
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
			for _, j := range right.eps[cur.j] {
				next := pair{cur.i, j}
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		out := make([]pair, 0, len(seen))
		for item := range seen {
			out = append(out, item)
		}
		return out
	}
	seen := map[pair]bool{}
	var queue []pair
	push := func(items []pair) {
		for _, item := range items {
			if !seen[item] {
				seen[item] = true
				queue = append(queue, item)
			}
		}
	}
	push(close(pair{}))
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.i == left.accept && cur.j == right.accept {
			return true
		}
		for s := range symbols {
			var moved []pair
			for _, i := range left.trans[cur.i][s] {
				for _, j := range right.trans[cur.j][s] {
					moved = append(moved, close(pair{i, j})...)
				}
			}
			push(moved)
		}
	}
	return false
}

func sha256Fingerprint(s string) bool {
	s = strings.ToLower(strings.ReplaceAll(s, ":", ""))
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

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

// FileError reports a file failure with a fixed description instead of the
// operating system's error text, so startup diagnostics follow the log rules.
func FileError(action string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s: file does not exist", action)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s: permission denied", action)
	}
	return fmt.Errorf("%s: file cannot be read", action)
}

var unknownField = regexp.MustCompile(`^json: unknown field "([A-Za-z0-9_]{1,64})"$`)

// JSONProblem describes a decoding failure without echoing decoder text:
// only a field name from the operator's own configuration, or a byte offset.
func JSONProblem(err error) string {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		return fmt.Sprintf("field %q has the wrong type", strings.Trim(nameUnsafe.ReplaceAllString(typeErr.Field, "_"), "_"))
	case errors.As(err, &syntaxErr):
		return fmt.Sprintf("syntax error at byte %d", syntaxErr.Offset)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "truncated document"
	}
	if match := unknownField.FindStringSubmatch(err.Error()); match != nil {
		return fmt.Sprintf("unknown field %q", match[1])
	}
	if strings.HasPrefix(err.Error(), "duplicate") || strings.HasPrefix(err.Error(), "trailing") || strings.HasPrefix(err.Error(), "unexpected delimiter") {
		return err.Error()
	}
	return "invalid JSON"
}

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.]`)

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

func (p *PVCs) validate() error {
	if err := uniqueNames("storageClasses", p.StorageClasses, true, Object); err != nil {
		return err
	}
	if err := uniqueNames("expectedNodes", p.ExpectedNodes, true, Object); err != nil {
		return err
	}
	if err := uniqueNamespaced("expected", p.Expected, false); err != nil {
		return err
	}
	if err := uniqueNamespaced("allowUnmounted", p.AllowUnmounted, false); err != nil {
		return err
	}
	if p.UsedPercent < 1 || p.UsedPercent > 100 {
		return fmt.Errorf("usedPercent must be between 1 and 100")
	}
	return seconds("maxStatsAgeSeconds", p.MaxStatsAgeSeconds, true)
}
