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

package deploy

import (
	"context"
	"fmt"
	"io"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
)

// Result is currently unused, but a stub for results that might be returned
// from a Deployer.Run()
type Result struct{}

// Deployer is the Deploy API of skaffold and responsible for deploying
// the build results to a Kubernetes cluster
type Deployer interface {
	// Deploy should ensure that the build results are deployed to the Kubernetes
	// cluster.
	Deploy(context.Context, io.Writer, *build.BuildResult) (*Result, error)

	// Dependencies returns a list of files that the deployer depends on.
	// In dev mode, a redeploy will be triggered
	Dependencies() ([]string, error)

	// Cleanup deletes what was deployed by calling Deploy.
	Cleanup(context.Context, io.Writer) error
}

type multiDeployer struct {
	deployers []Deployer
}

func NewMultiDeployer(deployers []Deployer) Deployer {
	return &multiDeployer{
		deployers: deployers,
	}
}

func (m *multiDeployer) Deploy(ctx context.Context, w io.Writer, b *build.BuildResult) (*Result, error) {
	for _, deployer := range m.deployers {
		_, err := deployer.Deploy(ctx, w, b)
		if err != nil {
			return nil, err
		}
	}

	return nil, nil
}

func (m *multiDeployer) Dependencies() ([]string, error) {
	allDeps := []string{}

	for _, deployer := range m.deployers {
		deps, err := deployer.Dependencies()
		if err != nil {
			return allDeps, err
		}

		allDeps = append(allDeps, deps...)
	}

	for _, dep := range allDeps {
		fmt.Println(dep)
	}

	return allDeps, nil
}

func (m *multiDeployer) Cleanup(ctx context.Context, w io.Writer) error {
	for _, deployer := range m.deployers {
		err := deployer.Cleanup(ctx, w)
		if err != nil {
			return err
		}
	}

	return nil
}

func JoinTagsToBuildResult(b []build.Build, params map[string]string) (map[string]build.Build, error) {
	imageToBuildResult := map[string]build.Build{}
	for _, build := range b {
		imageToBuildResult[build.ImageName] = build
	}

	paramToBuildResult := map[string]build.Build{}
	for param, imageName := range params {
		build, ok := imageToBuildResult[imageName]
		if !ok {
			return nil, fmt.Errorf("No build present for %s", imageName)
		}
		paramToBuildResult[param] = build
	}
	return paramToBuildResult, nil
}
