package controllers

import (
	"context"
	"testing"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	. "github.com/onsi/gomega"
	buildv1alpha1 "github.com/tomhuang12/build-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDockerBuildReconciler_BuildInstance(t *testing.T) {
	g := NewWithT(t)
	id := "builder-" + randStringRunes(5)

	err := createNamespace(id)
	g.Expect(err).NotTo(HaveOccurred(), "failed to create test namespace")

	artifactFile := "instance-" + randStringRunes(5)
	artifactChecksum, err := createArtifact(testServer, "testdata/app", artifactFile)
	g.Expect(err).ToNot(HaveOccurred())

	repositoryName := types.NamespacedName{
		Name:      randStringRunes(5),
		Namespace: id,
	}

	err = applyGitRepository(repositoryName, artifactFile, "main/"+artifactChecksum)
	g.Expect(err).NotTo(HaveOccurred())

	dockerBuildKey := types.NamespacedName{
		Name:      "inst-" + randStringRunes(5),
		Namespace: id,
	}

	repo := "podinfo" + randStringRunes(5)

	dockerBuild := &buildv1alpha1.DockerBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockerBuildKey.Name,
			Namespace: dockerBuildKey.Namespace,
		},
		Spec: buildv1alpha1.DockerBuildSpec{
			Interval: metav1.Duration{Duration: reconciliationInterval},
			Path:     "./testdata/app",
			SourceRef: buildv1alpha1.CrossNamespaceSourceReference{
				Name:      repositoryName.Name,
				Namespace: repositoryName.Namespace,
				Kind:      sourcev1.GitRepositoryKind,
			},
			ContainerRegistry: buildv1alpha1.ContainerRegistry{
				Repository:  repo,
				TagStrategy: buildv1alpha1.TagStrategyCommitSHA,
			},
		},
	}

	g.Expect(k8sClient.Create(context.TODO(), dockerBuild)).To(Succeed())

	g.Eventually(func() bool {
		var obj buildv1alpha1.DockerBuild
		_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(dockerBuild), &obj)
		return obj.Status.LastAppliedRevision == "main/"+artifactChecksum
	}, timeout, time.Second).Should(BeTrue())
}
