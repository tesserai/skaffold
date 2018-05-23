/*
Copyright 2018 Google LLC

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

package build

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/bazel"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/v1alpha2"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type DependencyMap struct {
	artifacts       []*v1alpha2.Artifact
	pathToArtifacts map[string][]*v1alpha2.Artifact
	artifactToPaths map[*v1alpha2.Artifact][]string
}

type DependencyResolver interface {
	GetDependencies(a *v1alpha2.Artifact) ([]string, error)
}

//TODO(@r2d4): Figure out best UX to support configuring this blacklist
var ignoredPrefixes = []string{"vendor", ".git"}

func (d *DependencyMap) Paths() []string {
	allPaths := []string{}
	for path := range d.pathToArtifacts {
		allPaths = append(allPaths, path)
	}
	sort.Strings(allPaths)
	return allPaths
}

func (d *DependencyMap) PathsForArtifact(artifact *v1alpha2.Artifact) []string {
	return d.artifactToPaths[artifact]
}

func (d *DependencyMap) ArtifactsForPaths(paths []string) []*v1alpha2.Artifact {
	m := map[*v1alpha2.Artifact]struct{}{}
	for _, p := range paths {
		artifacts := d.pathToArtifacts[p]
		for _, a := range artifacts {
			m[a] = struct{}{}
		}
	}
	artifacts := []*v1alpha2.Artifact{}
	for a := range m {
		artifacts = append(artifacts, a)
	}
	return artifacts
}

func NewDependencyMap(artifacts []*v1alpha2.Artifact) (*DependencyMap, error) {
	m, m2, err := pathToArtifactMap(artifacts)
	if err != nil {
		return nil, errors.Wrap(err, "generating path to artifact map")
	}
	return &DependencyMap{
		artifacts:       artifacts,
		pathToArtifacts: m,
		artifactToPaths: m2,
	}, nil
}

func isIgnored(path string) (bool, error) {
	for _, ignoredPrefix := range ignoredPrefixes {
		if strings.HasPrefix(path, ignoredPrefix) {
			return true, nil
		}
	}

	return false, nil
}

func pathToArtifactMap(artifacts []*v1alpha2.Artifact) (map[string][]*v1alpha2.Artifact, map[*v1alpha2.Artifact][]string, error) {
	m := map[string][]*v1alpha2.Artifact{}
	m2 := map[*v1alpha2.Artifact][]string{}

	for _, a := range artifacts {
		paths, err := pathsForArtifact(a)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "getting paths for artifact %s", a.ImageName)
		}

		m2[a] = paths

		for _, p := range paths {
			m[p] = append(m[p], a)
		}
	}

	return m, m2, nil
}

func pathsForArtifact(a *v1alpha2.Artifact) ([]string, error) {
	deps, err := GetDependenciesForArtifact(a)
	if err != nil {
		return nil, errors.Wrap(err, "getting dockerfile dependencies")
	}
	logrus.Infof("Source code dependencies %s: %s", a.ImageName, deps)

	var filteredDeps []string
	for _, dep := range deps {
		//TODO(r2d4): what does the ignore workspace look like for bazel?
		ignored, err := isIgnored(dep)
		if err != nil {
			return nil, errors.Wrapf(err, "calculating ignored files for artifact %s", a.ImageName)
		}
		if ignored {
			logrus.Debugf("Ignoring %s for artifact dependencies", dep)
			continue
		}
		filteredDeps = append(filteredDeps, filepath.Join(a.Workspace, dep))
	}
	return filteredDeps, nil
}

var (
	DefaultDockerfileDepResolver DependencyResolver = &docker.DockerfileDepResolver{}
	DefaultBazelDepResolver      DependencyResolver = &bazel.BazelDependencyResolver{}
)

func GetDependenciesForArtifact(artifact *v1alpha2.Artifact) ([]string, error) {
	if artifact.DockerArtifact != nil {
		return DefaultDockerfileDepResolver.GetDependencies(artifact)
	}
	if artifact.BazelArtifact != nil {
		return DefaultBazelDepResolver.GetDependencies(artifact)
	}

	return nil, fmt.Errorf("undefined artifact type: %+v", artifact.ArtifactType)
}
