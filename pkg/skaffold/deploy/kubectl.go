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
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/v1alpha2"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/docker/distribution/reference"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

// Slightly modified from kubectl run --dry-run
var deploymentTemplate = template.Must(template.New("deployment").Parse(`apiVersion: extensions/v1beta1
kind: Deployment
metadata:
  labels:
    run: skaffold
  name: skaffold
spec:
  replicas: 1
  selector:
    matchLabels:
      run: skaffold
  strategy: {}
  template:
    metadata:
      labels:
        run: skaffold
    spec:
      containers:
      - image: {{ .Image }}
        name: app
{{if .Ports}}
        ports:
{{range .Ports}}
        - containerPort: {{.}}
{{end}}
{{end}}
`))

// KubectlDeployer deploys workflows using kubectl CLI.
type KubectlDeployer struct {
	*v1alpha2.DeployConfig
	kubeContext string
}

// NewKubectlDeployer returns a new KubectlDeployer for a DeployConfig filled
// with the needed configuration for `kubectl apply`
func NewKubectlDeployer(cfg *v1alpha2.DeployConfig, kubeContext string) *KubectlDeployer {
	return &KubectlDeployer{
		DeployConfig: cfg,
		kubeContext:  kubeContext,
	}
}

// Deploy templates the provided manifests with a simple `find and replace` and
// runs `kubectl apply` on those manifests
func (k *KubectlDeployer) Deploy(ctx context.Context, out io.Writer, b *build.BuildResult) (*Result, error) {
	manifests, err := k.readOrGenerateManifests(b)
	if err != nil {
		return nil, errors.Wrap(err, "reading manifests")
	}

	manifests, err = manifests.replaceImages(b.Builds)
	if err != nil {
		return nil, errors.Wrap(err, "replacing images in manifests")
	}

	err = k.kubectl(manifests.reader(), out, "apply", "-f", "-")
	if err != nil {
		return nil, errors.Wrap(err, "deploying manifests")
	}

	return &Result{}, nil
}

// Cleanup deletes what was deployed by calling Deploy.
func (k *KubectlDeployer) Cleanup(ctx context.Context, out io.Writer) error {
	if len(k.KubectlDeploy.Manifests) == 0 {
		return k.kubectl(nil, out, "delete", "deployment", "skaffold")
	}

	manifests, err := k.readManifests()
	if err != nil {
		return errors.Wrap(err, "reading manifests")
	}

	err = k.kubectl(manifests.reader(), out, "delete", "-f", "-")
	if err != nil {
		return errors.Wrap(err, "deleting manifests")
	}

	return nil
}

// Not implemented
func (k *KubectlDeployer) Dependencies() ([]string, error) {
	return manifestFiles(k.KubectlDeploy.Manifests)
}

// readOrGenerateManifests reads the manifests to deploy/delete. If no manifest exists, try to
// generate it with the information we have.
func (k *KubectlDeployer) readOrGenerateManifests(b *build.BuildResult) (manifestList, error) {
	if len(k.KubectlDeploy.Manifests) > 0 {
		return k.readManifests()
	}

	if len(b.Builds) != 1 {
		return nil, errors.New("must specify manifest if using more than one image")
	}

	yaml, err := generateManifest(b.Builds[0])
	if err != nil {
		return nil, errors.Wrap(err, "generating manifest")
	}

	return manifestList{yaml}, nil
}

func (k *KubectlDeployer) kubectl(in io.Reader, out io.Writer, arg ...string) error {
	args := append([]string{"--context", k.kubeContext}, arg...)

	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out

	return util.RunCmd(cmd)
}

func manifestFiles(manifests []string) ([]string, error) {
	list, err := util.ExpandPathsGlob(manifests)
	if err != nil {
		return nil, errors.Wrap(err, "expanding kubectl manifest paths")
	}

	var filteredManifests []string
	for _, f := range list {
		if !util.IsSupportedKubernetesFormat(f) {
			if !util.StrSliceContains(manifests, f) {
				logrus.Infof("refusing to deploy/delete non {json, yaml} file %s", f)
				logrus.Info("If you still wish to deploy this file, please specify it directly, outside a glob pattern.")
				continue
			}
		}
		filteredManifests = append(filteredManifests, f)
	}

	return filteredManifests, nil
}

// readManifests reads the manifests to deploy/delete.
func (k *KubectlDeployer) readManifests() (manifestList, error) {
	files, err := manifestFiles(k.KubectlDeploy.Manifests)
	if err != nil {
		return nil, errors.Wrap(err, "expanding user manifest list")
	}
	var manifests manifestList

	for _, manifest := range files {
		buf, err := afero.ReadFile(util.Fs, manifest)
		if err != nil {
			return nil, errors.Wrap(err, "reading manifest")
		}

		parts := bytes.Split(buf, []byte("\n---"))
		for _, part := range parts {
			manifests = append(manifests, part)
		}
	}

	for _, m := range k.KubectlDeploy.RemoteManifests {
		manifest, err := k.readRemoteManifest(m)
		if err != nil {
			return nil, errors.Wrap(err, "get remote manifests")
		}

		manifests = append(manifests, manifest)
	}

	logrus.Debugln("manifests", manifests.String())

	return manifests, nil
}

func (k *KubectlDeployer) readRemoteManifest(name string) ([]byte, error) {
	var args []string
	if parts := strings.Split(name, ":"); len(parts) > 1 {
		args = append(args, "--namespace", parts[0])
		name = parts[1]
	}
	args = append(args, "get", name, "-o", "yaml")

	var manifest bytes.Buffer
	err := k.kubectl(nil, &manifest, args...)
	if err != nil {
		return nil, errors.Wrap(err, "getting manifest")
	}

	return manifest.Bytes(), nil
}

func generateManifest(b build.Build) ([]byte, error) {
	logrus.Info("No manifests specified. Generating a deployment.")

	dockerfilePath := filepath.Join(b.Artifact.Workspace, b.Artifact.DockerArtifact.DockerfilePath)
	r, err := os.Open(dockerfilePath)
	if err != nil {
		return nil, errors.Wrap(err, "reading dockerfile")
	}

	ports, err := docker.PortsFromDockerfile(r)
	if err != nil {
		logrus.Warnf("Unable to determine port from Dockerfile: %s.", err)
	}

	var out bytes.Buffer
	if err := deploymentTemplate.Execute(&out, struct {
		Ports []string
		Image string
	}{
		Ports: ports,
		Image: b.ImageName,
	}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type replacement struct {
	tag   string
	found bool
}

type manifestList [][]byte

func (l *manifestList) String() string {
	var str string
	for i, manifest := range *l {
		if i != 0 {
			str += "\n---\n"
		}
		str += string(bytes.TrimSpace(manifest))
	}
	return str
}

func (l *manifestList) reader() io.Reader {
	return strings.NewReader(l.String())
}

func (l *manifestList) replaceImages(b []build.Build) (manifestList, error) {
	replacements := map[string]*replacement{}
	for _, build := range b {
		replacements[build.ImageName] = &replacement{
			tag: build.Tag,
		}
	}

	var updatedManifests manifestList

	for _, manifest := range *l {
		m := make(map[interface{}]interface{})
		if err := yaml.Unmarshal(manifest, &m); err != nil {
			return nil, errors.Wrap(err, "reading kubernetes YAML")
		}

		if len(m) == 0 {
			continue
		}

		recursiveReplaceImage(m, replacements)

		updatedManifest, err := yaml.Marshal(m)
		if err != nil {
			return nil, errors.Wrap(err, "marshalling yaml")
		}

		updatedManifests = append(updatedManifests, updatedManifest)
	}

	for name, replacement := range replacements {
		if !replacement.found {
			logrus.Warnf("image [%s] is not used by the deployment", name)
		}
	}

	logrus.Debugln("manifests with tagged images", updatedManifests.String())

	return updatedManifests, nil
}

func recursiveReplaceImage(i interface{}, replacements map[string]*replacement) {
	switch t := i.(type) {
	case []interface{}:
		for _, v := range t {
			recursiveReplaceImage(v, replacements)
		}
	case map[interface{}]interface{}:
		for k, v := range t {
			if k.(string) == "image" {
				image := v.(string)
				parsed, err := parseReference(image)
				if err != nil {
					logrus.Warnf("Couldn't parse image: %s", v)
					continue
				}

				if parsed.fullyQualified {
					// TODO(1.0.0): Remove this warning.
					logrus.Infof("Not replacing fully qualified image: %s (see #565)", v)
					continue
				}

				if img, present := replacements[parsed.baseName]; present {
					t[k] = img.tag
					img.found = true
				}
			} else {
				if k.(string) == "env" && v.([]interface{}) != nil {
					for _, elt := range v.([]interface{}) {
						if elt.(map[interface{}]interface{}) == nil {
							break
						}

						envVarEntry := elt.(map[interface{}]interface{})
						envVarName := envVarEntry["name"].(string)
						if envVarName == "" {
							break
						}

						if strings.HasSuffix(envVarName, "_IMAGE") {
							image := envVarEntry["value"].(string)
							parsed, err := parseReference(image)
							if err != nil {
								logrus.Warnf("Couldn't parse image: %s", v)
								continue
							}

							if parsed.fullyQualified {
								// TODO(1.0.0): Remove this warning.
								logrus.Infof("Not replacing fully qualified image: %s (see #565)", v)
								continue
							}

							if img, present := replacements[parsed.baseName]; present {
								envVarEntry["value"] = img.tag
								img.found = true
							}
						}
					}
				}
				recursiveReplaceImage(v, replacements)
				continue
			}
		}
	}
}

type imageReference struct {
	baseName       string
	fullyQualified bool
}

func parseReference(image string) (*imageReference, error) {
	r, err := reference.Parse(image)
	if err != nil {
		return nil, err
	}

	baseName := image
	if n, ok := r.(reference.Named); ok {
		baseName = n.Name()
	}

	fullyQualified := false
	switch n := r.(type) {
	case reference.Tagged:
		fullyQualified = n.Tag() != "latest"
	case reference.Digested:
		fullyQualified = true
	}

	return &imageReference{
		baseName:       baseName,
		fullyQualified: fullyQualified,
	}, nil
}
