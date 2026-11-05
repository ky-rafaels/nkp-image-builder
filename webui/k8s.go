package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var errNotFound = errors.New("build not found")

// Build is the API view of a build Job.
type Build struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	OS          string        `json:"os"`
	ImageName   string        `json:"imageName"`
	Phase       string        `json:"phase"` // Pending, Running, Succeeded, Failed
	Message     string        `json:"message,omitempty"`
	CreatedAt   time.Time     `json:"createdAt"`
	StartedAt   *time.Time    `json:"startedAt,omitempty"`
	CompletedAt *time.Time    `json:"completedAt,omitempty"`
	Request     *BuildRequest `json:"request,omitempty"`
	PackerVars  string        `json:"packerVars,omitempty"`
}

type Builds struct {
	cfg    *Config
	client kubernetes.Interface
}

func (b *Builds) Create(ctx context.Context, req *BuildRequest) (*Build, error) {
	id := newBuildID()
	cm, secret, job, err := buildObjects(b.cfg, req, id)
	if err != nil {
		return nil, err
	}
	ns := b.cfg.Namespace

	if cm, err = b.client.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating build vars configmap: %w", err)
	}
	if secret != nil {
		if secret, err = b.client.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			b.cleanup(cm, nil)
			return nil, fmt.Errorf("creating build credentials secret: %w", err)
		}
	}
	created, err := b.client.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		b.cleanup(cm, secret)
		return nil, fmt.Errorf("creating build job: %w", err)
	}

	// Tie the ConfigMap/Secret lifetime to the Job so TTL cleanup or a cancel
	// removes them too.
	owner := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       created.Name,
		UID:        created.UID,
	}
	cm.OwnerReferences = []metav1.OwnerReference{owner}
	if _, err := b.client.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("setting owner on configmap: %w", err)
	}
	if secret != nil {
		secret.OwnerReferences = []metav1.OwnerReference{owner}
		if _, err := b.client.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("setting owner on secret: %w", err)
		}
	}
	out := toBuild(created)
	out.PackerVars = cm.Data[varsFileKey]
	return out, nil
}

func (b *Builds) cleanup(cm *corev1.ConfigMap, secret *corev1.Secret) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if cm != nil {
		_ = b.client.CoreV1().ConfigMaps(cm.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{})
	}
	if secret != nil {
		_ = b.client.CoreV1().Secrets(secret.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{})
	}
}

func (b *Builds) List(ctx context.Context) ([]*Build, error) {
	jobs, err := b.client.BatchV1().Jobs(b.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Build, 0, len(jobs.Items))
	for i := range jobs.Items {
		out = append(out, toBuild(&jobs.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (b *Builds) job(ctx context.Context, id string) (*batchv1.Job, error) {
	job, err := b.client.BatchV1().Jobs(b.cfg.Namespace).Get(ctx, "nkp-ib-"+id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || (err == nil && job.Labels[labelManagedBy] != managedByValue) {
		return nil, errNotFound
	}
	return job, err
}

func (b *Builds) Get(ctx context.Context, id string) (*Build, error) {
	job, err := b.job(ctx, id)
	if err != nil {
		return nil, err
	}
	out := toBuild(job)
	if cm, err := b.client.CoreV1().ConfigMaps(b.cfg.Namespace).Get(ctx, job.Name, metav1.GetOptions{}); err == nil {
		out.PackerVars = cm.Data[varsFileKey]
	}
	return out, nil
}

// Delete cancels (if running) and removes a build. The pod gets SIGTERM and
// its terminationGracePeriodSeconds to let Packer tear down the build VM.
// Foreground propagation keeps the Job visible (as "Cancelling") until the
// pod has actually exited, so the UI reflects the cleanup in progress.
func (b *Builds) Delete(ctx context.Context, id string) error {
	if _, err := b.job(ctx, id); err != nil {
		return err
	}
	policy := metav1.DeletePropagationForeground
	err := b.client.BatchV1().Jobs(b.cfg.Namespace).Delete(ctx, "nkp-ib-"+id, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if apierrors.IsNotFound(err) {
		return errNotFound
	}
	return err
}

// Logs streams the build pod's logs. When follow is set it blocks until the
// container exits or ctx is cancelled.
func (b *Builds) Logs(ctx context.Context, id string, follow bool, w io.Writer) error {
	if _, err := b.job(ctx, id); err != nil {
		return err
	}
	pod, err := b.waitForPod(ctx, id, follow)
	if err != nil {
		return err
	}
	stream, err := b.client.CoreV1().Pods(b.cfg.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "image-builder",
		Follow:    follow,
	}).Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = io.Copy(w, stream)
	return err
}

func (b *Builds) waitForPod(ctx context.Context, id string, wait bool) (*corev1.Pod, error) {
	for {
		pods, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelBuildID + "=" + id,
		})
		if err != nil {
			return nil, err
		}
		if len(pods.Items) > 0 {
			sort.Slice(pods.Items, func(i, j int) bool {
				return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
			})
			p := &pods.Items[0]
			if p.Status.Phase != corev1.PodPending {
				return p, nil
			}
			// Container may already have failed to start (bad image, missing secret).
			for _, cs := range p.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
					return nil, fmt.Errorf("build pod is waiting: %s: %s", w.Reason, w.Message)
				}
			}
		}
		if !wait {
			return nil, fmt.Errorf("build pod has not started yet")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func toBuild(job *batchv1.Job) *Build {
	b := &Build{
		ID:        job.Labels[labelBuildID],
		Name:      job.Name,
		OS:        job.Labels[labelOS],
		ImageName: job.Annotations[annotationImage],
		CreatedAt: job.CreationTimestamp.Time,
		Phase:     "Pending",
	}
	if raw := job.Annotations[annotationReq]; raw != "" {
		var req BuildRequest
		if json.Unmarshal([]byte(raw), &req) == nil {
			b.Request = &req
		}
	}
	if t := job.Status.StartTime; t != nil {
		b.StartedAt = &t.Time
	}
	// Active counts pods that haven't started yet (image pull, scheduling),
	// so only report Running once the build container is actually up.
	if job.Status.Ready != nil && *job.Status.Ready > 0 {
		b.Phase = "Running"
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			b.Phase = "Succeeded"
			t := c.LastTransitionTime.Time
			b.CompletedAt = &t
		case batchv1.JobFailed:
			b.Phase = "Failed"
			b.Message = c.Message
			if c.Reason != "" && c.Message == "" {
				b.Message = c.Reason
			}
			t := c.LastTransitionTime.Time
			b.CompletedAt = &t
		}
	}
	if job.Status.CompletionTime != nil && b.CompletedAt == nil {
		b.CompletedAt = &job.Status.CompletionTime.Time
	}
	if job.DeletionTimestamp != nil {
		b.Phase = "Cancelling"
	}
	return b
}
