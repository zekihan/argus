package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pgbackrestSection = `"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{},"repo2":{"maxAgeSeconds":36000}}}]}`

func load(t *testing.T, data string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestStrictConfiguration(t *testing.T) {
	valid := `{"checks":{` + pgbackrestSection + `}}`
	c, err := load(t, valid)
	if err != nil || c.Checks.PGBackRest.MaxAgeSeconds != 28800 || c.Checks.PGBackRest.Clusters[0].Repos["repo2"].MaxAgeSeconds != 36000 {
		t.Fatal(c, err)
	}
	for _, data := range []string{`{"checks":{"pgbackrest":{}}}`, `{"checks":{"pgbackrest":{"clusters":[]}}}`, `{"checks":{"pgbackrest":{"clusters":null}}}`,
		`{"unknown":true}`, `{"checks":{"unknown":{}}}`, `{"checks":{},"checks":{}}`, valid + valid,
		`{"checks":{"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{"maxAgeSeconds":-1}}}]}}}`,
		`{"checks":{"pgbackrest":{"clusters":[{"namespace":"db/other","name":"db","repos":{"repo1":{}}}]}}}`,
		`{"checks":{"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{"repo5":{}}}]}}}`,
		`{"checks":{"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{}}]}}}`,
		`{"checks":{"pgbackrest":{"unknown":1,"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}}}]}}}`} {
		if _, err := load(t, data); err == nil {
			t.Fatal("invalid config accepted", data)
		}
	}
	for _, mutate := range []func(*Config){
		func(c *Config) {
			c.Checks.PGBackRest.Clusters = append(c.Checks.PGBackRest.Clusters, c.Checks.PGBackRest.Clusters[0])
		},
		func(c *Config) { c.RequestTimeoutSeconds = 0 },
		func(c *Config) { c.PollIntervalSeconds = c.MaxSampleAgeSeconds },
		func(c *Config) { c.PollIntervalSeconds = 90; c.CollectionTimeoutSeconds = 45 },
		func(c *Config) { c.RequestTimeoutSeconds = c.CollectionTimeoutSeconds + 1 },
		func(c *Config) { c.ListenAddress = "invalid" },
		func(c *Config) { c.ListenAddress = ":99999" },
		func(c *Config) { c.ListenAddress = ":0" },
		func(c *Config) { c.ListenAddress = ":abc" },
		func(c *Config) { c.ListenAddress = ":" },
		func(c *Config) { c.ListenAddress = ":+8080" },
		func(c *Config) { c.TokenFile = "" },
		func(c *Config) { c.TokenFile = "relative" },
		func(c *Config) { c.TLS = &TLS{CertFile: "tls.crt", KeyFile: "/tls/tls.key"} },
		func(c *Config) { c.State.Name = "" },
		func(c *Config) { c.Checks.PGBackRest.IntervalSeconds = 170 },
	} {
		candidate, err := Parse([]byte(valid))
		if err != nil {
			t.Fatal(err)
		}
		mutate(&candidate)
		if candidate.Validate() == nil {
			t.Fatal(candidate)
		}
	}
}

func TestConfigurationAllowsTimingBudgetBelowSampleAge(t *testing.T) {
	c, err := Parse([]byte(`{"pollIntervalSeconds":89,"collectionTimeoutSeconds":45,"requestTimeoutSeconds":10,"maxSampleAgeSeconds":180,"checks":{` + pgbackrestSection + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	if interval, timeout, maxAge := c.Timing(c.Checks.PGBackRest.Schedule); interval != 89 || timeout != 45 || maxAge != 180 {
		t.Fatal(interval, timeout, maxAge)
	}
	c, err = Parse([]byte(`{"checks":{"pgbackrest":{"intervalSeconds":600,"maxSampleAgeSeconds":900,"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}}}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if interval, _, maxAge := c.Timing(c.Checks.PGBackRest.Schedule); interval != 600 || maxAge != 900 {
		t.Fatal(interval, maxAge)
	}
}

func TestConfigurationRejectsCaseInsensitiveDuplicates(t *testing.T) {
	const inventory = `"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}}}]`
	for name, data := range map[string]string{
		"top-level override":   `{"maxSampleAgeSeconds":3600,"MaxSampleAgeSeconds":7200}`,
		"check override":       `{"checks":{"pgbackrest":{"maxAgeSeconds":3600,"MaxAgeSeconds":7200,` + inventory + `}}}`,
		"two aliases":          `{"checks":{"pgbackrest":{"MAXAGESECONDS":3600,"MaxAgeSeconds":7200,` + inventory + `}}}`,
		"escaped alias":        `{"checks":{"pgbackrest":{"maxAgeSeconds":3600,"MaxAgeSeconds":7200,` + inventory + `}}}`,
		"Unicode alias":        `{"checks":{"pgbackrest":{"maxAgeSeconds":3600,"maxAgeſeconds":7200,` + inventory + `}}}`,
		"check section":        `{"checks":{"pgbackrest":{` + inventory + `},"PGBackRest":{` + inventory + `}}}`,
		"cluster namespace":    `{"checks":{"pgbackrest":{"clusters":[{"namespace":"db","Namespace":"other","name":"db","repos":{"repo1":{}}}]}}}`,
		"repository inventory": `{"checks":{"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}},"Repos":{"repo2":{}}}]}}}`,
		"repository override":  `{"checks":{"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{"maxAgeSeconds":3600,"MaxAgeSeconds":7200}}}]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, data); err == nil {
				t.Fatal("case-insensitive duplicate accepted", data)
			}
		})
	}
}

// Every field added after a chart release must be optional: the minimal
// sections below are what an early chart renders and must keep loading.
func TestMinimalSectionsLoadWithDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"checks":{
	"hosts":{"privateKeyFile":"/ssh/key","knownHostsFile":"/config/known_hosts","hosts":[{"name":"bastion","address":"192.0.2.1"},{"name":"nut","address":"nut.example.internal:2222","expects":["ups"]}]},
	"nodes":{"expected":["node-a"]},
	"postgres":{"clusters":[{"namespace":"db","name":"db"}]},
	` + pgbackrestSection + `,
	"longhorn":{"namespace":"longhorn","expectedPVCs":["app/data"],"maxBackupAgeSeconds":108000,"systemBackupMaxAgeSeconds":691200},
	"cronjobs":{"items":[{"namespace":"dmarc","name":"dmarc-converter","maxAgeSeconds":10800}]},
	"certificates":{},
	"pki":{"rootSHA256":"` + strings.Repeat("a", 64) + `","expectedCertificates":[{"namespace":"cert-manager","name":"selfsigned-ca"}]},
	"externalsecrets":{},
	"argocd":{"namespace":"argocd"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	k := c.Checks
	if len(c.Enabled()) != 10 || k.Hosts.User != "argus" || k.Hosts.Concurrency != 8 || k.Nodes.PersistSeconds != 7200 || !k.Nodes.MetricsEnabled() ||
		k.Longhorn.BackupGroup != "backup" || k.CronJobs.Items[0].MaxRunSeconds != 3600 || k.Certificates.ExpiryDays != 14 ||
		k.PKI.MarginDays != 90 || k.PKI.RootName != "selfsigned-ca" || k.ExternalSecrets.RefreshGraceSeconds != 600 || k.ArgoCD.ManualOutOfSyncSeconds != 604800 {
		t.Fatalf("%+v", k)
	}
	if address, err := HostAddress("192.0.2.1"); err != nil || address != "192.0.2.1:22" {
		t.Fatal(address, err)
	}
	empty, err := Parse([]byte(`{}`))
	if err != nil || len(empty.Enabled()) != 0 {
		t.Fatal("a configuration with every check disabled must load", err)
	}
}

func TestSectionValidation(t *testing.T) {
	for name, section := range map[string]string{
		"host name":          `"hosts":{"privateKeyFile":"/k","knownHostsFile":"/h","hosts":[{"name":"Bad Name","address":"192.0.2.1"}]}`,
		"host duplicate":     `"hosts":{"privateKeyFile":"/k","knownHostsFile":"/h","hosts":[{"name":"a","address":"192.0.2.1"},{"name":"a","address":"192.0.2.2"}]}`,
		"host expects":       `"hosts":{"privateKeyFile":"/k","knownHostsFile":"/h","hosts":[{"name":"a","address":"192.0.2.1","expects":["ssh"]}]}`,
		"host port":          `"hosts":{"privateKeyFile":"/k","knownHostsFile":"/h","hosts":[{"name":"a","address":"192.0.2.1:0"}]}`,
		"host key path":      `"hosts":{"privateKeyFile":"k","knownHostsFile":"/h","hosts":[{"name":"a","address":"192.0.2.1"}]}`,
		"nodes inventory":    `"nodes":{"expected":[]}`,
		"nodes percent":      `"nodes":{"expected":["a"],"cpuPercent":101}`,
		"postgres inventory": `"postgres":{"clusters":[]}`,
		"longhorn pvc":       `"longhorn":{"namespace":"longhorn","expectedPVCs":["data"],"maxBackupAgeSeconds":1,"systemBackupMaxAgeSeconds":1}`,
		"longhorn age":       `"longhorn":{"namespace":"longhorn","expectedPVCs":["a/b"],"maxBackupAgeSeconds":0,"systemBackupMaxAgeSeconds":1}`,
		"cronjob age":        `"cronjobs":{"items":[{"namespace":"a","name":"b","maxAgeSeconds":0}]}`,
		"cronjob duplicate":  `"cronjobs":{"items":[{"namespace":"a","name":"b","maxAgeSeconds":1},{"namespace":"a","name":"b","maxAgeSeconds":1}]}`,
		"pki digest":         `"pki":{"rootSHA256":"abc","expectedCertificates":[{"namespace":"a","name":"b"}]}`,
		"pki inventory":      `"pki":{"rootSHA256":"` + strings.Repeat("a", 64) + `","expectedCertificates":[]}`,
		"eso expected":       `"externalsecrets":{"expectedExternalSecrets":[{"namespace":"a"}]}`,
		"argocd namespace":   `"argocd":{"namespace":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(`{"checks":{` + section + `}}`)); err == nil {
				t.Fatal("invalid section accepted", section)
			}
		})
	}
}
