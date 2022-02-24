/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/acl"
	"github.com/fluxcd/pkg/runtime/metrics"
	"github.com/fluxcd/pkg/runtime/predicates"
	"github.com/fluxcd/pkg/untar"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	"github.com/hashicorp/go-retryablehttp"
	buildv1alpha1 "github.com/tomhuang12/build-controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dockertypes "github.com/docker/docker/api/types"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
)

// DockerBuildReconciler reconciles a DockerBuild object
type DockerBuildReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	MetricsRecorder      *metrics.Recorder
	NoCrossNamespaceRefs bool
	httpClient           *retryablehttp.Client
	ControllerName       string
}

// DockerBuildReconcilerOptions options
type DockerBuildReconcilerOptions struct {
	MaxConcurrentReconciles   int
	HTTPRetry                 int
	DependencyRequeueInterval time.Duration
}

//+kubebuilder:rbac:groups=build.contrib.flux.io,resources=dockerbuilds,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=build.contrib.flux.io,resources=dockerbuilds/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=build.contrib.flux.io,resources=dockerbuilds/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DockerBuild object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *DockerBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	reconcileStart := time.Now()

	var dockerBuild buildv1alpha1.DockerBuild
	if err := r.Get(ctx, req.NamespacedName, &dockerBuild); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// resolve source reference
	source, err := r.getSource(ctx, dockerBuild)
	if err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Source '%s' not found", dockerBuild.Spec.SourceRef.String())
			dockerBuild = buildv1alpha1.DockerBuildNotReady(dockerBuild, "", buildv1alpha1.ArtifactFailedReason, msg)
			if err := r.patchStatus(ctx, req, dockerBuild.Status); err != nil {
				return ctrl.Result{Requeue: true}, err
			}
			r.recordReadiness(ctx, dockerBuild)
			log.Info(msg)
			// do not requeue immediately, when the source is created the watcher should trigger a reconciliation
			return ctrl.Result{RequeueAfter: dockerBuild.GetRetryInterval()}, nil
		}

		// retry on transient errors
		return ctrl.Result{Requeue: true}, err

	}

	if source.GetArtifact() == nil {
		msg := "Source is not ready, artifact not found"
		dockerBuild = buildv1alpha1.DockerBuildNotReady(dockerBuild, "", buildv1alpha1.ArtifactFailedReason, msg)
		if err := r.patchStatus(ctx, req, dockerBuild.Status); err != nil {
			log.Error(err, "unable to update status for artifact not found")
			return ctrl.Result{Requeue: true}, err
		}
		r.recordReadiness(ctx, dockerBuild)
		log.Info(msg)
		// do not requeue immediately, when the artifact is created the watcher should trigger a reconciliation
		return ctrl.Result{RequeueAfter: dockerBuild.GetRetryInterval()}, nil
	}

	// record reconciliation duration
	if r.MetricsRecorder != nil {
		objRef, err := reference.GetReference(r.Scheme, &dockerBuild)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer r.MetricsRecorder.RecordDuration(*objRef, reconcileStart)
	}

	// set the reconciliation status to progressing
	dockerBuild = buildv1alpha1.DockerBuildProgressing(dockerBuild, "reconciliation in progress")
	if err := r.patchStatus(ctx, req, dockerBuild.Status); err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	r.recordReadiness(ctx, dockerBuild)

	// reconcile dockerBuild by applying the latest revision
	reconciledDockerBuild, reconcileErr := r.reconcile(ctx, *dockerBuild.DeepCopy(), source)
	if err := r.patchStatus(ctx, req, reconciledDockerBuild.Status); err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	r.recordReadiness(ctx, reconciledDockerBuild)

	// broadcast the reconciliation failure and requeue at the specified retry interval
	if reconcileErr != nil {
		log.Error(reconcileErr, fmt.Sprintf("Reconciliation failed after %s, next try in %s",
			time.Since(reconcileStart).String(),
			dockerBuild.GetRetryInterval().String()),
			"revision",
			source.GetArtifact().Revision)

		return ctrl.Result{RequeueAfter: dockerBuild.GetRetryInterval()}, nil
	}

	// broadcast the reconciliation result and requeue at the specified interval
	msg := fmt.Sprintf("Reconciliation finished in %s, next run in %s",
		time.Since(reconcileStart).String(),
		dockerBuild.Spec.Interval.Duration.String())
	log.Info(msg, "revision", source.GetArtifact().Revision)

	return ctrl.Result{RequeueAfter: dockerBuild.Spec.Interval.Duration}, nil
}

func (r *DockerBuildReconciler) reconcile(ctx context.Context, dockerBuild buildv1alpha1.DockerBuild, source sourcev1.Source) (buildv1alpha1.DockerBuild, error) {
	if v, ok := meta.ReconcileAnnotationValue(dockerBuild.GetAnnotations()); ok {
		dockerBuild.Status.SetLastHandledReconcileRequest(v)
	}

	revision := source.GetArtifact().Revision

	// create tmp dir
	tmpDir, err := os.MkdirTemp("", dockerBuild.Name)
	if err != nil {
		err = fmt.Errorf("tmp dir error: %w", err)
		return buildv1alpha1.DockerBuildNotReady(
			dockerBuild,
			revision,
			sourcev1.StorageOperationFailedReason,
			err.Error(),
		), err
	}
	defer os.RemoveAll(tmpDir)

	// download artifact and extract files
	err = r.download(source.GetArtifact(), tmpDir)
	if err != nil {
		return buildv1alpha1.DockerBuildNotReady(
			dockerBuild,
			revision,
			buildv1alpha1.ArtifactFailedReason,
			err.Error(),
		), err
	}

	// check build path exists
	dirPath, err := securejoin.SecureJoin(tmpDir, dockerBuild.Spec.Path)
	if err != nil {
		return buildv1alpha1.DockerBuildNotReady(
			dockerBuild,
			revision,
			buildv1alpha1.ArtifactFailedReason,
			err.Error(),
		), err
	}
	if _, err := os.Stat(dirPath); err != nil {
		err = fmt.Errorf("dockerBuild path not found: %w", err)
		return buildv1alpha1.DockerBuildNotReady(
			dockerBuild,
			revision,
			buildv1alpha1.ArtifactFailedReason,
			err.Error(),
		), err
	}

	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv)
	if err != nil {
		return buildv1alpha1.DockerBuildNotReady(
			dockerBuild,
			revision,
			buildv1alpha1.BuildFailedReason,
			err.Error(),
		), err
	}

	// build the docker image
	err = r.build(ctx, cli, revision, dirPath, &dockerBuild)
	if err != nil {
		return buildv1alpha1.DockerBuildNotReady(
			dockerBuild,
			revision,
			buildv1alpha1.BuildFailedReason,
			err.Error(),
		), err
	}

	return buildv1alpha1.DockerBuildReady(
		dockerBuild,
		revision,
		meta.ReconciliationSucceededReason,
		fmt.Sprintf("Applied revision: %s", revision),
	), err
}

func (r *DockerBuildReconciler) build(ctx context.Context, cli *dockerclient.Client, revision, dir string, dockerBuild *buildv1alpha1.DockerBuild) error {
	dockerCtx, cancel := context.WithTimeout(context.Background(), time.Second*120)
	defer cancel()

	tar, err := archive.TarWithOptions(dir, &archive.TarOptions{})
	if err != nil {
		return err
	}

	imageTag, err := getImageTag(*dockerBuild, revision)
	if err != nil {
		return err
	}

	opts := dockertypes.ImageBuildOptions{
		Dockerfile: "Dockerfile",
		Tags:       []string{imageTag},
		Remove:     true,
	}
	res, err := cli.ImageBuild(dockerCtx, tar, opts)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	err = logDockerResponse(ctx, res.Body)
	if err != nil {
		return err
	}

	return nil
}

func logDockerResponse(ctx context.Context, rb io.Reader) error {
	var lastLine string
	log := ctrl.LoggerFrom(ctx)

	scanner := bufio.NewScanner(rb)
	for scanner.Scan() {
		lastLine = scanner.Text()
		log.Info(scanner.Text())
	}

	errLine := &buildv1alpha1.ErrorLine{}
	json.Unmarshal([]byte(lastLine), errLine)
	if errLine.Error != "" {
		return errors.New(errLine.Error)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func getImageTag(dockerBuild buildv1alpha1.DockerBuild, revision string) (string, error) {
	// drop any non-alphanumeric characters
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		return "", err
	}
	newRevision := reg.ReplaceAllString(revision, "")

	switch dockerBuild.Spec.ContainerRegistry.TagStrategy {
	case buildv1alpha1.TagStrategyCommitSHA:
		if len(revision) > 7 {
			return fmt.Sprintf("%s:%s", dockerBuild.Spec.ContainerRegistry.Repository, newRevision[0:8]), nil
		} else {
			return fmt.Sprintf("%s:%s", dockerBuild.Spec.ContainerRegistry.Repository, newRevision), nil
		}
	}
	return "", fmt.Errorf("invalid tag strategy")
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	const (
		gitRepositoryIndexKey string = ".metadata.gitRepository"
	)

	// Configure the retryable http client used for fetching artifacts.
	// By default it retries 10 times within a 3.5 minutes window.
	httpClient := retryablehttp.NewClient()
	httpClient.RetryWaitMin = 5 * time.Second
	httpClient.RetryWaitMax = 30 * time.Second
	httpClient.RetryMax = 5
	httpClient.Logger = nil
	r.httpClient = httpClient

	// Index the DockerBuild by the GitRepository references they (may) point at.
	if err := mgr.GetCache().IndexField(context.TODO(), &buildv1alpha1.DockerBuild{}, gitRepositoryIndexKey,
		r.indexBy(sourcev1.GitRepositoryKind)); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&buildv1alpha1.DockerBuild{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		Watches(
			&source.Kind{Type: &sourcev1.GitRepository{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForRevisionChangeOf(buildv1alpha1.GitRepositoryIndexKey)),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Complete(r)
}

func (r *DockerBuildReconciler) indexBy(kind string) func(o client.Object) []string {
	return func(o client.Object) []string {
		build, ok := o.(*buildv1alpha1.DockerBuild)
		if !ok {
			panic(fmt.Sprintf("Expected a Kustomization, got %T", o))
		}

		if build.Spec.SourceRef.Kind == kind {
			namespace := build.GetNamespace()
			if build.Spec.SourceRef.Namespace != "" {
				namespace = build.Spec.SourceRef.Namespace
			}
			return []string{fmt.Sprintf("%s/%s", namespace, build.Spec.SourceRef.Name)}
		}

		return nil
	}
}

func (r *DockerBuildReconciler) getSource(ctx context.Context, dockerBuild buildv1alpha1.DockerBuild) (sourcev1.Source, error) {
	var source sourcev1.Source
	sourceNamespace := dockerBuild.GetNamespace()
	if dockerBuild.Spec.SourceRef.Namespace != "" {
		sourceNamespace = dockerBuild.Spec.SourceRef.Namespace
	}

	namespacedName := types.NamespacedName{
		Namespace: sourceNamespace,
		Name:      dockerBuild.Spec.SourceRef.Name,
	}

	if r.NoCrossNamespaceRefs && sourceNamespace != dockerBuild.GetNamespace() {
		return source, acl.AccessDeniedError(
			fmt.Sprintf("can't access '%s/%s', cross-namespace references have been blocked",
				dockerBuild.Spec.SourceRef.Kind, namespacedName))
	}

	switch dockerBuild.Spec.SourceRef.Kind {
	case sourcev1.GitRepositoryKind:
		var repository sourcev1.GitRepository
		err := r.Client.Get(ctx, namespacedName, &repository)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return source, err
			}
			return source, fmt.Errorf("unable to get source '%s': %w", namespacedName, err)
		}
		source = &repository
	default:
		return source, fmt.Errorf("source `%s` kind '%s' not supported",
			dockerBuild.Spec.SourceRef.Name, dockerBuild.Spec.SourceRef.Kind)
	}
	return source, nil
}

func (r *DockerBuildReconciler) requestsForRevisionChangeOf(indexKey string) func(obj client.Object) []reconcile.Request {
	return func(obj client.Object) []reconcile.Request {
		repo, ok := obj.(interface {
			GetArtifact() *sourcev1.Artifact
		})
		if !ok {
			panic(fmt.Sprintf("Expected an object conformed with GetArtifact() method, but got a %T", obj))
		}
		// If we do not have an artifact, we have no requests to make
		if repo.GetArtifact() == nil {
			return nil
		}

		ctx := context.Background()
		var list buildv1alpha1.DockerBuildList
		if err := r.List(ctx, &list, client.MatchingFields{
			indexKey: client.ObjectKeyFromObject(obj).String(),
		}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, len(list.Items))
		for i, t := range list.Items {
			// If the revision of the artifact equals to the last attempted revision,
			// we should not make a request for this Terraform
			if repo.GetArtifact().Revision == t.Status.LastAttemptedRevision {
				continue
			}
			reqs[i].NamespacedName.Name = t.Name
			reqs[i].NamespacedName.Namespace = t.Namespace
		}
		return reqs
	}

}

func (r *DockerBuildReconciler) recordReadiness(ctx context.Context, dockerBuild buildv1alpha1.DockerBuild) {
	if r.MetricsRecorder == nil {
		return
	}
	log := ctrl.LoggerFrom(ctx)

	objRef, err := reference.GetReference(r.Scheme, &dockerBuild)
	if err != nil {
		log.Error(err, "unable to record readiness metric")
		return
	}
	if rc := apimeta.FindStatusCondition(dockerBuild.Status.Conditions, meta.ReadyCondition); rc != nil {
		r.MetricsRecorder.RecordCondition(*objRef, *rc, !dockerBuild.DeletionTimestamp.IsZero())
	} else {
		r.MetricsRecorder.RecordCondition(*objRef, metav1.Condition{
			Type:   meta.ReadyCondition,
			Status: metav1.ConditionUnknown,
		}, !dockerBuild.DeletionTimestamp.IsZero())
	}
}

func (r *DockerBuildReconciler) patchStatus(ctx context.Context, req ctrl.Request, newStatus buildv1alpha1.DockerBuildStatus) error {
	var dockerBuild buildv1alpha1.DockerBuild
	if err := r.Get(ctx, req.NamespacedName, &dockerBuild); err != nil {
		return err
	}

	patch := client.MergeFrom(dockerBuild.DeepCopy())
	dockerBuild.Status = newStatus

	return r.Status().Patch(ctx, &dockerBuild, patch)
}

func (r *DockerBuildReconciler) download(artifact *sourcev1.Artifact, tmpDir string) error {
	artifactURL := artifact.URL
	if hostname := os.Getenv("SOURCE_CONTROLLER_LOCALHOST"); hostname != "" {
		u, err := url.Parse(artifactURL)
		if err != nil {
			return err
		}
		u.Host = hostname
		artifactURL = u.String()
	}

	req, err := retryablehttp.NewRequest(http.MethodGet, artifactURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create a new request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download artifact, error: %w", err)
	}
	defer resp.Body.Close()

	// check response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download artifact from %s, status: %s", artifactURL, resp.Status)
	}

	var buf bytes.Buffer

	// verify checksum matches origin
	if err := r.verifyArtifact(artifact, &buf, resp.Body); err != nil {
		return err
	}

	// extract
	if _, err = untar.Untar(&buf, tmpDir); err != nil {
		return fmt.Errorf("failed to untar artifact, error: %w", err)
	}

	return nil
}

func (r *DockerBuildReconciler) verifyArtifact(artifact *sourcev1.Artifact, buf *bytes.Buffer, reader io.Reader) error {
	hasher := sha256.New()

	// for backwards compatibility with source-controller v0.17.2 and older
	if len(artifact.Checksum) == 40 {
		hasher = sha1.New()
	}

	// compute checksum
	mw := io.MultiWriter(hasher, buf)
	if _, err := io.Copy(mw, reader); err != nil {
		return err
	}

	if checksum := fmt.Sprintf("%x", hasher.Sum(nil)); checksum != artifact.Checksum {
		return fmt.Errorf("failed to verify artifact: computed checksum '%s' doesn't match advertised '%s'",
			checksum, artifact.Checksum)
	}

	return nil
}
