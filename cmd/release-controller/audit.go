package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type AuditStore interface {
	PutSignature(ctx context.Context, dgst string, signature []byte) error
	HasSignature(dgst string) bool
}

// sync expects to receive a queue key that points to a valid release image input
// or to the entire namespace.
func (c *Controller) syncAudit(key queueKey) error {
	defer func() {
		err := recover()
		panic(err)
	}()

	release, err := c.loadReleaseForSync(key.namespace, key.name)
	if err != nil || release == nil {
		return err
	}

	glog.V(4).Infof("Audit %s", release.Config.Name)
	c.auditTracker.Sync(release)
	return nil
}

func (c *Controller) syncAuditTag(releaseName string) error {
	record, ok := c.auditTracker.Get(releaseName)
	if !ok {
		return nil
	}

	if record.Failure != nil {
		glog.V(4).Infof("Release already failed, ignoring until retry interval is up")
		return nil
	}

	if len(record.ID) == 0 {
		msg := fmt.Sprintf("Release %s has no digest and cannot be verified", record.Name)
		c.auditTracker.SetFailure(record.Name, msg)
		glog.V(4).Info(msg)
		return nil
	}

	release, err := c.loadReleaseForSync(record.ImageStreamNamespace, record.ImageStreamName)
	if err != nil || release == nil {
		return err
	}

	if c.auditStore.HasSignature(record.ID) {
		glog.V(5).Infof("Release %s (%s) is already signed", record.ID, record.Name)
		return nil
	}

	// we allow the auditor to pin to a specific CLI image for safety when verifying
	image := c.cliImageForAudit
	if len(image) == 0 {
		image = release.Config.OverrideCLIImage
	}
	if len(image) == 0 {
		glog.Warningf("Unable to audit release %s, no configured audit CLI image or overrideCLIImage defined on the stream", releaseName)
		return nil
	}

	if image == "local" {
		out, err := exec.Command("oc", "adm", "release", "info", "--verify", record.Location).CombinedOutput()
		if err != nil {
			failureMsg := fmt.Sprintf("Unable to verify release:\n%s", strings.TrimSpace(string(out)))
			glog.V(4).Infof("Release verification command failed: %s", failureMsg)
			c.auditTracker.SetFailure(record.Name, failureMsg)
			return nil
		}

	} else {
		if count, ok := c.countAuditVerifyJobs(); !ok || count > 2 {
			glog.V(4).Infof("Throttling verify jobs to max 2")
			c.auditQueue.AddAfter(releaseName, 10*time.Second)
			return nil
		}

		job, err := c.ensureAuditVerifyJob(release, record)
		if err != nil || job == nil {
			return fmt.Errorf("unable to verify release before signing: %v", err)
		}

		success, complete := jobIsComplete(job)
		switch {
		case !complete:
			c.auditQueue.AddAfter(releaseName, 10*time.Second)
			return nil

		case !success:
			failureMsg := "Unable to verify release for unknown reason"
			if message, _, _ := ensureJobTerminationMessageRetrieved(c.podClient, job, "status.phase=Failed", "verify", false); len(message) > 0 {
				failureMsg = fmt.Sprintf("Unable to verify release:\n\n%s", message)
			}
			glog.V(4).Infof("Release verification job failed: %s", failureMsg)
			c.auditTracker.SetFailure(record.Name, failureMsg)
			return nil
		}
	}

	switch {
	case c.signer == nil:
		glog.V(4).Infof("Completed audit of %s at %s without signing", releaseName, release.Source.ResourceVersion)
		return nil

	default:
		sig, err := c.signer.Sign(record.ID, record.Location)
		if err != nil {
			return fmt.Errorf("unable to sign release: %v", err)
		}
		if glog.V(5) {
			glog.Infof("Signed:\n%s", hex.Dump(sig))
		}
		ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelFn()
		if err := c.auditStore.PutSignature(ctx, record.ID, sig); err != nil {
			return fmt.Errorf("unable to upload release signature: %v", err)
		}
		glog.V(4).Infof("Signed and uploaded signature for %s (%s)", record.ID, record.Name)
	}

	return nil
}

var auditVerifyJobSelector = labels.SelectorFromSet(labels.Set{releaseAnnotationJobPurpose: "audit"})

func (c *Controller) countAuditVerifyJobs() (int, bool) {
	result, err := c.jobLister.Jobs(c.jobNamespace).List(auditVerifyJobSelector)
	if err != nil {
		return 0, false
	}
	count := 0
	for _, job := range result {
		if job.Status.CompletionTime != nil {
			continue
		}
		count++
	}
	return count, true
}

func (c *Controller) ensureAuditVerifyJob(release *Release, record *AuditRecord) (*batchv1.Job, error) {
	// create a safe job name
	name := record.ID
	parts := strings.SplitN(record.ID, ":", 2)
	if len(parts) == 2 {
		name = fmt.Sprintf("verify-%s", parts[1])
	}
	name = strings.Replace(name, ":", "-", -1)
	if len(name) > 63 {
		name = name[:63]
	}

	return c.ensureJob(name, nil, func() (*batchv1.Job, error) {
		cliImage := release.Config.OverrideCLIImage

		job, prefix := newReleaseJobBase(name, cliImage, release.Config.PullSecretName)

		// copy the contents of the release to the mirror
		job.Spec.Template.Spec.Containers[0].Name = "verify"
		job.Spec.Template.Spec.Containers[0].Command = []string{
			"/bin/bash", "-c",
			prefix + `
			oc adm release info --verify "$1"
			`,
			"",
			record.Location,
		}

		if job.Labels == nil {
			job.Labels = make(map[string]string)
		}
		job.Labels[releaseAnnotationJobPurpose] = "audit"
		job.Annotations[releaseAnnotationSource] = fmt.Sprintf("%s/%s", release.Source.Namespace, release.Source.Name)
		job.Annotations[releaseAnnotationTarget] = fmt.Sprintf("%s/%s", release.Target.Namespace, release.Target.Name)
		job.Annotations[releaseAnnotationReleaseTag] = record.Name
		job.Annotations[releaseAnnotationJobPurpose] = "audit"

		glog.V(2).Infof("Running release verify job for %s (%s)", record.ID, record.Name)
		return job, nil
	})
}

func ensureJobTerminationMessageRetrieved(podClient kv1core.PodsGetter, job *batchv1.Job, podFieldSelector, containerName string, onlySuccess bool) (string, int, bool) {
	if job.Status.Active == 0 {
		glog.V(4).Infof("Deferring pod lookup for %s - no active pods", job.Name)
		return "", 0, false
	}
	statuses, err := findJobContainerStatus(podClient, job, podFieldSelector, containerName)
	if err != nil {
		return "", 0, false
	}
	// put the most recently terminated first
	sort.Slice(statuses, func(i, j int) bool {
		// a and b are reversed, so that we reverse the sort
		a, b := statuses[j], statuses[i]
		if a.State.Terminated != nil && b.State.Terminated != nil {
			return a.State.Terminated.FinishedAt.Time.Before(b.State.Terminated.FinishedAt.Time)
		}
		if a.State.Terminated == nil {
			return true
		}
		if b.State.Terminated == nil {
			return false
		}
		return false
	})
	// Take the first message and exit code on a terminated container, which should be
	// the most recent. If we only want successful, we can go deeper in the list.
	for _, status := range statuses {
		if status.State.Terminated == nil {
			continue
		}
		if onlySuccess && status.State.Terminated.ExitCode != 0 {
			continue
		}
		return status.State.Terminated.Message, int(status.State.Terminated.ExitCode), true
	}
	return "", 0, false
}

type AuditTracker struct {
	lock    sync.Mutex
	records map[string]*AuditRecord
	queue   workqueue.DelayingInterface
}

type AuditRecord struct {
	At       time.Time
	Name     string
	ID       string
	Location string

	Release              string
	ImageStreamNamespace string
	ImageStreamName      string

	Failure *AuditFailure
}

type AuditFailure struct {
	Reason  string
	Message string
}

func NewAuditTracker(queue workqueue.DelayingInterface) *AuditTracker {
	return &AuditTracker{
		records: make(map[string]*AuditRecord),
		queue:   queue,
	}
}

func (a *AuditTracker) SetFailure(name string, message string) {
	a.lock.Lock()
	defer a.lock.Unlock()

	existing, ok := a.records[name]
	if !ok {
		return
	}
	existing.At = time.Now()
	existing.Failure = &AuditFailure{
		Reason:  "VerificationFailed",
		Message: message,
	}
}

func (a *AuditTracker) Get(name string) (*AuditRecord, bool) {
	a.lock.Lock()
	defer a.lock.Unlock()

	existing, ok := a.records[name]
	if !ok {
		return nil, false
	}
	copied := *existing
	if existing.Failure != nil {
		failureCopied := *existing.Failure
		copied.Failure = &failureCopied
	}
	return &copied, true
}

func (a *AuditTracker) Sync(release *Release) {
	if release.Config.As != releaseConfigModeStable {
		return
	}

	a.lock.Lock()
	defer a.lock.Unlock()

	// add or update tags
	now := time.Now()
	found := sets.NewString()
	from := release.Target
	for _, tag := range from.Spec.Tags {
		if _, ok := tag.Annotations[releaseAnnotationSource]; !ok {
			continue
		}
		if len(tag.Name) == 0 {
			continue
		}
		phase := tag.Annotations[releaseAnnotationPhase]
		if phase != "Accepted" && phase != "Ready" && phase != "Rejected" {
			continue
		}

		found.Insert(tag.Name)

		id := findImageIDForTag(from, tag.Name)
		// TODO: this should really be the digest
		location := findPublicImagePullSpec(from, tag.Name)
		existing, ok := a.records[tag.Name]
		if !ok {
			a.records[tag.Name] = &AuditRecord{
				At:       now,
				Name:     tag.Name,
				ID:       id,
				Location: location,

				Release:              release.Config.Name,
				ImageStreamName:      release.Source.Name,
				ImageStreamNamespace: release.Source.Namespace,
			}
			a.queue.Add(tag.Name)
			glog.V(5).Infof("Saw %s for the first time", tag.Name)
			continue
		}
		changed := false
		if existing.Location != location {
			glog.Warningf("Location of %s changed from %s to %s", tag.Name, existing.Location, location)
			changed = true
		}
		if existing.ID != id {
			glog.Warningf("ID of %s changed from %s to %s", tag.Name, existing.ID, id)
			changed = true
		}
		if time.Now().Sub(existing.At) > 12*time.Hour {
			existing.At = now
			existing.Failure = nil
			changed = true
		}
		if changed {
			a.queue.Add(tag.Name)
		}
	}

	// remove old tags
	for k, v := range a.records {
		if v.Release != release.Config.Name {
			continue
		}
		if !found.Has(k) {
			glog.Warningf("Release tag %s deleted", k)
			delete(a.records, k)
		}
	}
}

type imageStreamStore struct {
	store cache.Store
}

func NewImageStreamStore() *imageStreamStore {
	return &imageStreamStore{
		store: cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil),
	}
}
