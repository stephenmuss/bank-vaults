// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"strings"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/heroku/docker-registry-client/registry"
	log "github.com/sirupsen/logrus"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var logger log.FieldLogger

func init() {
	logger = log.New()
}

// BlobResponse stores blob response
type BlobResponse struct {
	Config Config `json:"config"`
}

// Config stores Cmd and Entrypoint retrieved from blob response
type Config struct {
	Cmd        []string `json:"Cmd"`
	Entrypoint []string `json:"Entrypoint"`
}

type DockerCreds struct {
	Auths map[string]dockerTypes.AuthConfig `json:"auths"`
}

// GetImageBlob download image blob from registry
func GetImageBlob(url, username, password, image string) ([]string, []string) {
	imageName, tag := ParseContainerImage(image)

	registrySkipVerify := os.Getenv("REGISTRY_SKIP_VERIFY")

	var hub *registry.Registry
	var err error

	if registrySkipVerify == "true" {
		hub, err = registry.NewInsecure(url, username, password)
	} else {
		hub, err = registry.New(url, username, password)
	}
	if err != nil {
		logger.Fatal("Cannot create client for registry", zap.Error(err))
	}

	manifest, err := hub.ManifestV2(imageName, tag)
	if err != nil {
		logger.Fatal("Cannot download manifest for image", zap.Error(err))
	}

	reader, err := hub.DownloadBlob(imageName, manifest.Config.Digest)
	if reader != nil {
		defer reader.Close()
	}
	if err != nil {
		logger.Fatal("Cannot download blob", zap.Error(err))
	}

	b, err := ioutil.ReadAll(reader)
	if err != nil {
		logger.Fatal("Cannot read blob", zap.Error(err))
	}

	var msg BlobResponse
	err = json.Unmarshal(b, &msg)
	if err != nil {
		logger.Fatal("Cannot unmarshal JSON", zap.Error(err))
	}

	return msg.Config.Entrypoint, msg.Config.Cmd
}

// ParseContainerImage returns image and tag
func ParseContainerImage(image string) (string, string) {
	split := strings.SplitN(image, ":", 2)

	if len(split) <= 1 {
		logger.Fatal("Cannot find tag for image", zap.String("image", image))
	}

	imageName := split[0]
	tag := split[1]

	return imageName, tag
}

// GetEntrypointCmd returns entrypoint and command of container
func GetEntrypointCmd(clientset *kubernetes.Clientset, namespace string, container *corev1.Container, podSpec *corev1.PodSpec) ([]string, []string) {
	podInfo := K8s{Namespace: namespace, clientset: clientset}
	podInfo.Load(container, podSpec)

	if podInfo.RegistryName != "" {
		logger.Info("Trimmed registry name from image name",
			zap.String("registry", podInfo.RegistryName),
			zap.String("image", podInfo.Image),
		)
		podInfo.Image = strings.TrimLeft(podInfo.Image, fmt.Sprintf("%s/", podInfo.RegistryName))
	}

	registryAddress := podInfo.RegistryAddress
	if registryAddress == "" {
		registryAddress = "https://registry-1.docker.io/"
	}
	logger.Infoln("I'm using registry", registryAddress, podInfo.RegistryUsername, podInfo.RegistryPassword)

	return GetImageBlob(registryAddress, podInfo.RegistryUsername, podInfo.RegistryPassword, podInfo.Image)
}

// K8s structure keeps information retrieved from POD definition
type K8s struct {
	clientset        *kubernetes.Clientset
	Namespace        string
	ImagePullSecrets string
	RegistryAddress  string
	RegistryName     string
	RegistryUsername string
	RegistryPassword string
	Image            string
}

func (k *K8s) readDockerSecret(namespace, secretName string) (map[string][]byte, error) {
	secret, err := k.clientset.CoreV1().Secrets(namespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return secret.Data, nil
}

func (k *K8s) parseDockerConfig(dockerCreds DockerCreds) {
	k.RegistryName = reflect.ValueOf(dockerCreds.Auths).MapKeys()[0].String()
	if !strings.HasPrefix(k.RegistryName, "https://") {
		k.RegistryAddress = fmt.Sprintf("https://%s", k.RegistryName)
	} else {
		k.RegistryAddress = k.RegistryName
	}

	auths := dockerCreds.Auths
	k.RegistryUsername = auths[k.RegistryName].Username
	k.RegistryPassword = auths[k.RegistryName].Password
}

// Load reads information from k8s and load them into the structure
func (k *K8s) Load(container *corev1.Container, podSpec *corev1.PodSpec) {

	k.Image = container.Image

	if len(podSpec.ImagePullSecrets) >= 1 {
		k.ImagePullSecrets = podSpec.ImagePullSecrets[0].Name

		if k.ImagePullSecrets != "" {
			data, err := k.readDockerSecret(k.Namespace, k.ImagePullSecrets)
			if err != nil {
				logger.Fatal("Cannot read imagePullSecrets", err)
			}
			dockerConfig := data[corev1.DockerConfigJsonKey]
			//parse config
			var dockerCreds DockerCreds
			err = json.Unmarshal(dockerConfig, &dockerCreds)
			if err != nil {
				logger.Fatal("Cannot unmarshal docker configuration from imagePullSecrets", err)
			}
			k.parseDockerConfig(dockerCreds)
		}
	}
}
