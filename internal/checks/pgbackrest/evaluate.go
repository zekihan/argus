package pgbackrest

import (
	"errors"
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

var rfc3339 = regexp.MustCompile(
	`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-](0\d|1\d|2[0-3]):[0-5]\d)$`,
)

func timestamp(value string, now time.Time) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !rfc3339.MatchString(value) || parsed.After(now) || parsed.IsZero() {
		return time.Time{}, errors.New("malformed or future timestamp")
	}
	return parsed.UTC(), nil
}

// buildClusters indexes the sample's clusters by namespace/name with creation times.
func buildClusters(
	sample Sample,
	now time.Time,
) (map[string]kube.Cluster, map[string]time.Time, error) {
	clusters := map[string]kube.Cluster{}
	created := map[string]time.Time{}
	for _, cluster := range sample.Clusters {
		meta := cluster.Metadata
		key := meta.Namespace + "/" + meta.Name
		if !config.Name(meta.Namespace) || !config.Name(meta.Name) || meta.UID == "" {
			return nil, nil, errors.New("invalid PostgresCluster identity")
		}
		if _, duplicate := clusters[key]; duplicate {
			return nil, nil, errors.New("duplicate PostgresCluster in API sample")
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
				return nil, nil, fmt.Errorf(
					"ambiguous PostgresCluster repository destination: %s",
					key,
				)
			}
			if !config.Repo(repo.Name) || repos[repo.Name] {
				return nil, nil, fmt.Errorf(
					"invalid or duplicate PostgresCluster repository: %s",
					key,
				)
			}
			repos[repo.Name] = true
		}
		clusters[key], created[key] = cluster, birth
	}
	return clusters, created, nil
}

func Evaluate(
	c config.PGBackRest,
	sample Sample,
	previous state.Ledger,
	now time.Time,
) (map[string]Result, state.Ledger, error) {
	clusters, created, err := buildClusters(sample, now)
	if err != nil {
		return nil, nil, err
	}
	results := map[string]Result{}
	next := state.Ledger{}
	expectedClusters := evaluateExpected(c, sample, clusters, created, previous, results, next, now)
	for key, cluster := range clusters {
		if !expectedClusters[key] && len(cluster.Spec.Backups.PGBackRest.Repos) == 0 {
			results[key+"/inventory"] = Result{
				Reason: "PostgresCluster is not in the expected inventory",
			}
		}
		for _, repo := range cluster.Spec.Backups.PGBackRest.Repos {
			if _, expected := results[key+"/"+repo.Name]; !expected {
				results[key+"/"+repo.Name] = Result{
					Reason: "Backup repository is not in the expected inventory",
				}
			}
		}
	}
	// Keep a tombstone for repositories removed from configured inventory. An
	// aggregate status can outlive the inventory change, so its old success must
	// not become eligible again if the repository is later re-added.
	carryTombstones(clusters, created, previous, next, now)
	// The evidence store refuses checkpoints larger than 4,000 entries. Fail
	// closed before Save so we never write a checkpoint that cannot be loaded.
	if len(next) > 4000 {
		return results, next, errors.New("evidence ledger exceeds the 4000-entry limit")
	}
	return results, next, nil
}

// evaluateExpected evaluates every configured cluster/repo pair.
func evaluateExpected(
	c config.PGBackRest,
	sample Sample,
	clusters map[string]kube.Cluster,
	created map[string]time.Time,
	previous state.Ledger,
	results map[string]Result,
	next state.Ledger,
	now time.Time,
) map[string]bool {
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
			result := missingClusterResult(
				sample,
				key,
				clusterKey,
				repo,
				cluster,
				found,
				previous,
				next,
				created,
				now,
				maxAge,
			)
			results[key] = result
		}
	}
	return expectedClusters
}

// carryMissing keeps a barrier for a missing cluster's previous entry.
func carryMissing(key string, previous, next state.Ledger, now time.Time) {
	old := previous[key]
	if old.UID == "" {
		return
	}
	old.LastSuccess, old.Source = time.Time{}, ""
	if now.After(old.NotBefore) {
		old.NotBefore = now
	}
	if old.Created.After(old.NotBefore) {
		old.NotBefore = old.Created
	}
	old.RepositoryID = stateMissing
	next[key] = old
}

// missingClusterResult evaluates one key, handling missing clusters and repos.
func missingClusterResult(
	sample Sample,
	key, clusterKey, repo string,
	cluster kube.Cluster,
	found bool,
	previous, next state.Ledger,
	created map[string]time.Time,
	now time.Time,
	maxAge int,
) Result {
	result := Result{MaxAgeSeconds: maxAge}
	if !found {
		result.Reason = "Expected PostgresCluster is missing"
		carryMissing(key, previous, next, now)
		return result
	}
	if !hasRepo(cluster, repo) {
		result.Reason = "Expected backup repository is missing"
		boundary := now
		if old := previous[key]; old.UID == cluster.Metadata.UID &&
			old.NotBefore.After(boundary) {
			boundary = old.NotBefore
		}
		next[key] = state.Entry{
			UID:          cluster.Metadata.UID,
			Created:      created[clusterKey],
			RepositoryID: stateMissing,
			NotBefore:    boundary,
		}
		return result
	}
	res, entry := evaluateRepository(
		sample,
		cluster,
		repo,
		created[clusterKey],
		previous[key],
		now,
		maxAge,
	)
	if entry.UID == "" {
		delete(next, key)
	} else {
		next[key] = entry
	}
	return res
}

// carryTombstones keeps barrier entries for keys removed from configured inventory.
func carryTombstones(
	clusters map[string]kube.Cluster,
	created map[string]time.Time,
	previous, next state.Ledger,
	now time.Time,
) {
	for key, old := range previous {
		if _, present := next[key]; present {
			continue
		}
		parts := strings.Split(key, "/")
		if len(parts) != 3 || !config.Name(parts[0]) || !config.Name(parts[1]) ||
			!config.Repo(parts[2]) {
			continue
		}
		clusterKey, repo := parts[0]+"/"+parts[1], parts[2]
		cluster, found := clusters[clusterKey]
		next[key] = tombstoneEntry(cluster, created[clusterKey], found, old, repo, now)
	}
}

// tombstoneEntry advances the barrier for a removed repository key.
func tombstoneEntry(
	cluster kube.Cluster,
	birth time.Time,
	found bool,
	old state.Entry,
	repo string,
	now time.Time,
) state.Entry {
	entry := old
	entry.LastSuccess, entry.Source = time.Time{}, ""
	if !found {
		// Treat an absent cluster as an unknown destination. Reappearance
		// then advances the barrier even if UID and backend later match again.
		entry.RepositoryID = stateMissing
		if now.After(entry.NotBefore) {
			entry.NotBefore = now
		}
		if entry.Created.After(entry.NotBefore) {
			entry.NotBefore = entry.Created
		}
		return entry
	}
	identity := stateMissing
	if hasRepo(cluster, repo) {
		identity = repositoryIdentity(cluster, repo)
	}
	if old.UID != cluster.Metadata.UID || !old.Created.Equal(birth) ||
		old.RepositoryID != identity ||
		!old.LastSuccess.IsZero() ||
		old.NotBefore.IsZero() {
		if now.After(entry.NotBefore) {
			entry.NotBefore = now
		}
	}
	if birth.After(entry.NotBefore) {
		entry.NotBefore = birth
	}
	entry.UID, entry.Created, entry.RepositoryID = cluster.Metadata.UID, birth, identity
	return entry
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
	return meta.Namespace == cluster.Metadata.Namespace &&
		meta.Labels[kube.Prefix+"cluster"] == cluster.Metadata.Name &&
		meta.Labels[kube.Prefix+"pgbackrest-repo"] == repo &&
		meta.Labels[kube.Prefix+"pgbackrest-backup"] != ""
}

func clusterOwner(owner kube.Owner, cluster kube.Cluster) bool {
	return owner.Controller && owner.Kind == "PostgresCluster" &&
		owner.APIVersion == kube.ClusterAPI &&
		owner.Name == cluster.Metadata.Name &&
		owner.UID != "" &&
		owner.UID == cluster.Metadata.UID
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
				if cron.Metadata.UID == owner.UID && cron.Metadata.Name == owner.Name &&
					labelsMatch(cron.Metadata, cluster, repo) &&
					cron.Metadata.Labels[kube.Prefix+"pgbackrest-backup"] == "scheduled" &&
					cronOwned(cron, cluster) {
					return true
				}
			}
		}
		return false
	}
	return true
}

// advanceBarrier carries the barrier state from the previous entry.
func advanceBarrier(
	cluster kube.Cluster,
	created time.Time,
	identity string,
	old state.Entry,
	now time.Time,
) (state.Entry, bool) {
	best := state.Entry{UID: cluster.Metadata.UID, Created: created, RepositoryID: identity}
	sameCluster := old.UID == cluster.Metadata.UID && old.Created.Equal(created)
	if !sameCluster {
		return best, false
	}
	if old.RepositoryID != identity {
		// Keep the boundary even when no new success is available, including across
		// restarts. Old jobs may still complete after the destination changed.
		best.NotBefore = now
		if old.NotBefore.After(best.NotBefore) {
			best.NotBefore = old.NotBefore
		}
		return best, false
	}
	best.NotBefore = old.NotBefore
	if !old.LastSuccess.IsZero() &&
		(old.LastSuccess.Before(created) || old.Source == "" || (!old.NotBefore.IsZero() && !old.LastSuccess.After(old.NotBefore))) {
		return best, true
	}
	best = old
	// A clock rollback makes retained evidence temporarily unusable,
	// but must not erase it from the next durable checkpoint.
	return best, old.NotBefore.After(now) || old.LastSuccess.After(now)
}

// repoEval holds the mutable evaluation state.
type repoEval struct {
	best     state.Entry
	baseline state.Entry
	created  time.Time
	now      time.Time
	invalid  bool
}

func (e *repoEval) eligibleStart(value string) bool {
	if e.best.NotBefore.IsZero() {
		return true
	}
	started, err := timestamp(value, e.now)
	return err == nil && started.After(e.best.NotBefore)
}

func (e *repoEval) accept(value, source string) {
	completed, err := timestamp(value, e.now)
	if err != nil {
		e.invalid = true
		return
	}
	if completed.Before(e.created) ||
		(!e.best.NotBefore.IsZero() && !completed.After(e.best.NotBefore)) {
		return
	}
	if e.best.LastSuccess.IsZero() || completed.After(e.best.LastSuccess) {
		e.best.LastSuccess, e.best.Source = completed, source
	}
}

// scanJobs accepts eligible completed Job evidence.
func (e *repoEval) scanJobs(sample Sample, cluster kube.Cluster, repo string) {
	for _, job := range sample.Jobs {
		if !labelsMatch(job.Metadata, cluster, repo) || job.Status.Succeeded <= 0 ||
			!jobOwned(job, cluster, repo, sample.CronJobs) {
			continue
		}
		complete := false
		for _, condition := range job.Status.Conditions {
			complete = complete || (condition.Type == "Complete" && condition.Status == "True")
		}
		if !complete {
			continue
		}
		birth, err := timestamp(job.Metadata.CreationTimestamp, e.now)
		if err != nil {
			e.invalid = true
			continue
		}
		if birth.Before(e.created) || !e.eligibleStart(job.Metadata.CreationTimestamp) {
			continue
		}
		completed, err := timestamp(job.Status.CompletionTime, e.now)
		if err != nil || completed.Before(birth) {
			e.invalid = true
			continue
		}
		e.accept(job.Status.CompletionTime, "Completed Job")
	}
}

// scanCrons accepts eligible CronJob lastSuccessfulTime evidence.
func (e *repoEval) scanCrons(sample Sample, cluster kube.Cluster, repo string) {
	for _, cron := range sample.CronJobs {
		if !labelsMatch(cron.Metadata, cluster, repo) ||
			cron.Metadata.Labels[kube.Prefix+"pgbackrest-backup"] != "scheduled" ||
			!cronOwned(cron, cluster) ||
			cron.Status.LastSuccessfulTime == "" {
			continue
		}
		birth, err := timestamp(cron.Metadata.CreationTimestamp, e.now)
		if err != nil {
			e.invalid = true
			continue
		}
		if birth.Before(e.created) || !e.eligibleStart(cron.Metadata.CreationTimestamp) {
			continue
		}
		completed, err := timestamp(cron.Status.LastSuccessfulTime, e.now)
		if err != nil {
			e.invalid = true
			continue
		}
		if completed.Before(birth) {
			e.invalid = true
			continue
		}
		e.accept(cron.Status.LastSuccessfulTime, "CronJob lastSuccessfulTime")
	}
}

func evaluateRepository(
	sample Sample,
	cluster kube.Cluster,
	repo string,
	created time.Time,
	old state.Entry,
	now time.Time,
	maxAge int,
) (Result, state.Entry) {
	result := Result{MaxAgeSeconds: maxAge, ClusterUID: cluster.Metadata.UID}
	best, invalid := advanceBarrier(
		cluster,
		created,
		repositoryIdentity(cluster, repo),
		old,
		now,
	)
	baseline := best
	eval := repoEval{best: best, baseline: baseline, created: created, now: now, invalid: invalid}
	// Aggregate status does not identify the Job or destination. After a change,
	// even its startTime is insufficient: Kubernetes resets Job startTime on resume.
	// Require Job creation or a newly created CronJob until fresh evidence is retained.
	if best.NotBefore.IsZero() {
		for _, backup := range cluster.Status.PGBackRest.ScheduledBackups {
			if backup.Repo == repo && backup.Succeeded > 0 {
				eval.accept(backup.CompletionTime, "PostgresCluster scheduled-backup status")
			}
		}
	}

	eval.scanJobs(sample, cluster, repo)
	eval.scanCrons(sample, cluster, repo)
	if eval.invalid {
		result.Reason = "Invalid successful backup completion timestamp"
		// Keep only the current destination's validated state or change barrier.
		return result, baseline
	}
	best = eval.best
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
