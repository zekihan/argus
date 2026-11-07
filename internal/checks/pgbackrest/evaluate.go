package pgbackrest

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/state"
)

type Result struct {
	Healthy       bool       `json:"healthy"`
	Reason        string     `json:"reason,omitempty"`
	MaxAgeSeconds int        `json:"maxAgeSeconds,omitempty"`
	LastSuccess   *time.Time `json:"lastSuccess,omitempty"`
	AgeSeconds    *float64   `json:"ageSeconds,omitempty"`
	Source        string     `json:"source,omitempty"`
	ClusterUID    string     `json:"clusterUID,omitempty"`
}

var rfc3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-](0\d|1\d|2[0-3]):[0-5]\d)$`)

func timestamp(value string, now time.Time) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !rfc3339.MatchString(value) || parsed.After(now) || parsed.IsZero() {
		return time.Time{}, fmt.Errorf("malformed or future timestamp")
	}
	return parsed.UTC(), nil
}

func Evaluate(c config.PGBackRest, sample Sample, previous state.Ledger, now time.Time) (map[string]Result, state.Ledger, error) {
	clusters := map[string]kube.Cluster{}
	created := map[string]time.Time{}
	for _, cluster := range sample.Clusters {
		meta := cluster.Metadata
		key := meta.Namespace + "/" + meta.Name
		if !config.Name(meta.Namespace) || !config.Name(meta.Name) || meta.UID == "" {
			return nil, nil, fmt.Errorf("invalid PostgresCluster identity")
		}
		if _, duplicate := clusters[key]; duplicate {
			return nil, nil, fmt.Errorf("duplicate PostgresCluster in API sample")
		}
		birth, err := timestamp(meta.CreationTimestamp, now)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid PostgresCluster creation timestamp: %s", key)
		}
		repos := map[string]bool{}
		for _, repo := range cluster.Spec.Backups.PGBackRest.Repos {
			backends := 0
			for _, present := range []bool{repo.S3 != nil, repo.GCS != nil, repo.Azure != nil, repo.Volume != nil} {
				if present {
					backends++
				}
			}
			if backends > 1 {
				return nil, nil, fmt.Errorf("ambiguous PostgresCluster repository destination: %s", key)
			}
			if !config.Repo(repo.Name) || repos[repo.Name] {
				return nil, nil, fmt.Errorf("invalid or duplicate PostgresCluster repository: %s", key)
			}
			repos[repo.Name] = true
		}
		clusters[key], created[key] = cluster, birth
	}
	results := map[string]Result{}
	next := state.Ledger{}
	expectedClusters := map[string]bool{}
	for _, expected := range c.Clusters {
		clusterKey := expected.Namespace + "/" + expected.Name
		expectedClusters[clusterKey] = true
		cluster, found := clusters[clusterKey]
		for repo, override := range expected.Repos {
			key := clusterKey + "/" + repo
			maxAge := c.MaxAgeSeconds
			if override.MaxAgeSeconds > 0 {
				maxAge = override.MaxAgeSeconds
			}
			result := Result{MaxAgeSeconds: maxAge}
			if !found {
				result.Reason = "Expected PostgresCluster is missing"
				if old := previous[key]; old.UID != "" {
					old.LastSuccess, old.Source = time.Time{}, ""
					if now.After(old.NotBefore) {
						old.NotBefore = now
					}
					if old.Created.After(old.NotBefore) {
						old.NotBefore = old.Created
					}
					old.RepositoryID = "missing"
					next[key] = old
				}
			} else if !hasRepo(cluster, repo) {
				result.Reason = "Expected backup repository is missing"
				boundary := now
				if old := previous[key]; old.UID == cluster.Metadata.UID && old.NotBefore.After(boundary) {
					boundary = old.NotBefore
				}
				next[key] = state.Entry{UID: cluster.Metadata.UID, Created: created[clusterKey], RepositoryID: "missing", NotBefore: boundary}
			} else {
				result, next[key] = evaluateRepository(sample, cluster, repo, created[clusterKey], previous[key], now, maxAge)
				if next[key].UID == "" {
					delete(next, key)
				}
			}
			results[key] = result
		}
	}
	for key, cluster := range clusters {
		if !expectedClusters[key] && len(cluster.Spec.Backups.PGBackRest.Repos) == 0 {
			results[key+"/inventory"] = Result{Reason: "PostgresCluster is not in the expected inventory"}
		}
		for _, repo := range cluster.Spec.Backups.PGBackRest.Repos {
			if _, expected := results[key+"/"+repo.Name]; !expected {
				results[key+"/"+repo.Name] = Result{Reason: "Backup repository is not in the expected inventory"}
			}
		}
	}
	// Keep a tombstone for repositories removed from configured inventory. An
	// aggregate status can outlive the inventory change, so its old success must
	// not become eligible again if the repository is later re-added.
	for key, old := range previous {
		if _, present := next[key]; present {
			continue
		}
		parts := strings.Split(key, "/")
		if len(parts) != 3 || !config.Name(parts[0]) || !config.Name(parts[1]) || !config.Repo(parts[2]) {
			continue
		}
		clusterKey, repo := parts[0]+"/"+parts[1], parts[2]
		entry := old
		entry.LastSuccess, entry.Source = time.Time{}, ""
		cluster, found := clusters[clusterKey]
		if found {
			birth := created[clusterKey]
			identity := "missing"
			if hasRepo(cluster, repo) {
				identity = repositoryIdentity(cluster, repo)
			}
			if old.UID != cluster.Metadata.UID || !old.Created.Equal(birth) || old.RepositoryID != identity ||
				!old.LastSuccess.IsZero() || old.NotBefore.IsZero() {
				if now.After(entry.NotBefore) {
					entry.NotBefore = now
				}
			}
			if birth.After(entry.NotBefore) {
				entry.NotBefore = birth
			}
			entry.UID, entry.Created, entry.RepositoryID = cluster.Metadata.UID, birth, identity
		} else {
			// Treat an absent cluster as an unknown destination. Reappearance
			// then advances the barrier even if UID and backend later match again.
			entry.RepositoryID = "missing"
			if now.After(entry.NotBefore) {
				entry.NotBefore = now
			}
			if entry.Created.After(entry.NotBefore) {
				entry.NotBefore = entry.Created
			}
		}
		next[key] = entry
	}
	// The evidence store refuses checkpoints larger than 4,000 entries. Fail
	// closed before Save so we never write a checkpoint that cannot be loaded.
	if len(next) > 4000 {
		return results, next, fmt.Errorf("evidence ledger exceeds the 4000-entry limit")
	}
	return results, next, nil
}

func hasRepo(cluster kube.Cluster, repo string) bool {
	for _, item := range cluster.Spec.Backups.PGBackRest.Repos {
		if item.Name == repo {
			return true
		}
	}
	return false
}

func labelsMatch(meta kube.Metadata, cluster kube.Cluster, repo string) bool {
	return meta.Namespace == cluster.Metadata.Namespace && meta.Labels[kube.Prefix+"cluster"] == cluster.Metadata.Name &&
		meta.Labels[kube.Prefix+"pgbackrest-repo"] == repo && meta.Labels[kube.Prefix+"pgbackrest-backup"] != ""
}

func clusterOwner(owner kube.Owner, cluster kube.Cluster) bool {
	return owner.Controller && owner.Kind == "PostgresCluster" && owner.APIVersion == kube.ClusterAPI &&
		owner.Name == cluster.Metadata.Name && owner.UID != "" && owner.UID == cluster.Metadata.UID
}

func cronOwned(cron kube.CronJob, cluster kube.Cluster) bool {
	for _, owner := range cron.Metadata.Owners {
		if clusterOwner(owner, cluster) {
			return true
		}
	}
	return false
}

func jobOwned(job kube.Job, cluster kube.Cluster, repo string, crons []kube.CronJob) bool {
	// Some PGO versions create Jobs without a controller owner. In that case,
	// matching labels AND an object created within this cluster's lifetime are required.
	for _, owner := range job.Metadata.Owners {
		if !owner.Controller {
			continue
		}
		if clusterOwner(owner, cluster) {
			return true
		}
		if owner.Kind == "CronJob" && owner.APIVersion == "batch/v1" && owner.UID != "" {
			for _, cron := range crons {
				if cron.Metadata.UID == owner.UID && cron.Metadata.Name == owner.Name && labelsMatch(cron.Metadata, cluster, repo) &&
					cron.Metadata.Labels[kube.Prefix+"pgbackrest-backup"] == "scheduled" && cronOwned(cron, cluster) {
					return true
				}
			}
		}
		return false
	}
	return true
}

func evaluateRepository(sample Sample, cluster kube.Cluster, repo string, created time.Time, old state.Entry, now time.Time, maxAge int) (Result, state.Entry) {
	result := Result{MaxAgeSeconds: maxAge, ClusterUID: cluster.Metadata.UID}
	identity := repositoryIdentity(cluster, repo)
	best := state.Entry{UID: cluster.Metadata.UID, Created: created, RepositoryID: identity}
	invalid := false
	sameCluster := old.UID == cluster.Metadata.UID && old.Created.Equal(created)
	if sameCluster {
		if old.RepositoryID != identity {
			// Keep the boundary even when no new success is available, including across
			// restarts. Old jobs may still complete after the destination changed.
			best.NotBefore = now
			if old.NotBefore.After(best.NotBefore) {
				best.NotBefore = old.NotBefore
			}
		} else {
			best.NotBefore = old.NotBefore
			if !old.LastSuccess.IsZero() && (old.LastSuccess.Before(created) || old.Source == "" || (!old.NotBefore.IsZero() && !old.LastSuccess.After(old.NotBefore))) {
				invalid = true
			} else {
				best = old
				// A clock rollback makes retained evidence temporarily unusable,
				// but must not erase it from the next durable checkpoint.
				invalid = old.NotBefore.After(now) || old.LastSuccess.After(now)
			}
		}
	}
	baseline := best
	eligibleStart := func(value string) bool {
		if best.NotBefore.IsZero() {
			return true
		}
		started, err := timestamp(value, now)
		return err == nil && started.After(best.NotBefore)
	}
	accept := func(value, source string) {
		completed, err := timestamp(value, now)
		if err != nil {
			invalid = true
			return
		}
		if completed.Before(created) || (!best.NotBefore.IsZero() && !completed.After(best.NotBefore)) {
			return
		}
		if best.LastSuccess.IsZero() || completed.After(best.LastSuccess) {
			best.LastSuccess, best.Source = completed, source
		}
	}
	// Aggregate status does not identify the Job or destination. After a change,
	// even its startTime is insufficient: Kubernetes resets Job startTime on resume.
	// Require Job creation or a newly created CronJob until fresh evidence is retained.
	if best.NotBefore.IsZero() {
		for _, backup := range cluster.Status.PGBackRest.ScheduledBackups {
			if backup.Repo == repo && backup.Succeeded > 0 {
				accept(backup.CompletionTime, "PostgresCluster scheduled-backup status")
			}
		}
	}

	for _, job := range sample.Jobs {
		if !labelsMatch(job.Metadata, cluster, repo) || job.Status.Succeeded <= 0 || !jobOwned(job, cluster, repo, sample.CronJobs) {
			continue
		}
		complete := false
		for _, condition := range job.Status.Conditions {
			complete = complete || (condition.Type == "Complete" && condition.Status == "True")
		}
		if !complete {
			continue
		}
		birth, err := timestamp(job.Metadata.CreationTimestamp, now)
		if err != nil {
			invalid = true
			continue
		}
		if birth.Before(created) || !eligibleStart(job.Metadata.CreationTimestamp) {
			continue
		}
		completed, err := timestamp(job.Status.CompletionTime, now)
		if err != nil || completed.Before(birth) {
			invalid = true
			continue
		}
		accept(job.Status.CompletionTime, "Completed Job")
	}
	for _, cron := range sample.CronJobs {
		if !labelsMatch(cron.Metadata, cluster, repo) || cron.Metadata.Labels[kube.Prefix+"pgbackrest-backup"] != "scheduled" ||
			!cronOwned(cron, cluster) || cron.Status.LastSuccessfulTime == "" {
			continue
		}
		birth, err := timestamp(cron.Metadata.CreationTimestamp, now)
		if err != nil {
			invalid = true
			continue
		}
		if birth.Before(created) || !eligibleStart(cron.Metadata.CreationTimestamp) {
			continue
		}
		completed, err := timestamp(cron.Status.LastSuccessfulTime, now)
		if err != nil {
			invalid = true
			continue
		}
		if completed.Before(birth) {
			invalid = true
			continue
		}
		accept(cron.Status.LastSuccessfulTime, "CronJob lastSuccessfulTime")
	}
	if invalid {
		result.Reason = "Invalid successful backup completion timestamp"
		// Keep only the current destination's validated state or change barrier.
		return result, baseline
	}
	if best.LastSuccess.IsZero() {
		result.Reason = "No successful completed backup recorded"
		return result, best
	}
	age := now.Sub(best.LastSuccess).Seconds()
	result.LastSuccess, result.AgeSeconds, result.Source = &best.LastSuccess, &age, best.Source
	result.Healthy = age <= float64(maxAge)
	if !result.Healthy {
		result.Reason = "Last successful backup exceeds maximum age"
	}
	return result, best
}
