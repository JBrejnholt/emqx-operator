/*
Copyright 2021.

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

package v2alpha1

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	emperror "emperror.dev/errors"

	appsv2alpha1 "github.com/emqx/emqx-operator/apis/apps/v2alpha1"
	"github.com/emqx/emqx-operator/internal/apiclient"
	innerErr "github.com/emqx/emqx-operator/internal/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type requestAPI struct {
	Username string
	Password string
	Port     string
	client.Client
	APIClient *apiclient.APIClient
}

func newRequestAPI(r *EMQXReconciler, instance *appsv2alpha1.EMQX) *requestAPI {
	var username, password, port string
	username, password, err := getBootstrapUser(r.Client, instance)
	if err != nil {
		r.EventRecorder.Event(instance, corev1.EventTypeWarning, "FailedToGetBootStrapUserSecret", err.Error())
	}

	dashboardPort, err := appsv2alpha1.GetDashboardServicePort(instance)
	if err != nil {
		msg := fmt.Sprintf("Failed to get dashboard service port: %s, use 18083 port", err.Error())
		r.EventRecorder.Event(instance, corev1.EventTypeWarning, "FailedToGetDashboardServicePort", msg)
		port = "18083"
	}
	if dashboardPort != nil {
		port = dashboardPort.TargetPort.String()
	}

	return &requestAPI{
		Username:  username,
		Password:  password,
		Port:      port,
		Client:    r.Client,
		APIClient: r.APIClient,
	}
}

func getBootstrapUser(k8sClient client.Client, instance *appsv2alpha1.EMQX) (username, password string, err error) {
	secret := &corev1.Secret{}
	if err = k8sClient.Get(context.TODO(), types.NamespacedName{Name: instance.NameOfBootStrapUser(), Namespace: instance.Namespace}, secret); err != nil {
		return "", "", err
	}

	data, ok := secret.Data["bootstrap_user"]
	if !ok {
		return "", "", emperror.Errorf("the secret does not contain the bootstrap_user")
	}

	str := string(data)
	index := strings.Index(str, ":")

	return str[:index], str[index+1:], nil
}

func (r *requestAPI) requestAPI(obj client.Object, method, path string, body []byte) (*http.Response, []byte, error) {
	list := &corev1.PodList{}
	err := r.Client.List(context.Background(), list, client.InNamespace(obj.GetNamespace()), client.MatchingLabels(obj.GetLabels()))
	if err != nil {
		return nil, nil, emperror.Wrap(err, "failed to list pods")
	}
	for _, pod := range list.Items {
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name == EMQXContainerName {
				if container.Ready {
					return r.APIClient.RequestAPI(&pod, r.Username, r.Password, r.Port, method, path, body)
				}
			}
		}
	}
	return nil, nil, innerErr.ErrPodNotReady
}

func (r *requestAPI) getNodeStatuesByAPI(obj client.Object) ([]appsv2alpha1.EMQXNode, error) {
	resp, body, err := r.requestAPI(obj, "GET", "api/v5/nodes", nil)
	if err != nil {
		return nil, emperror.Wrap(err, "failed to get API api/v5/nodes")
	}
	if resp.StatusCode != 200 {
		return nil, emperror.Errorf("failed to get API %s, status : %s, body: %s", "api/v5/nodes", resp.Status, body)
	}

	nodeStatuses := []appsv2alpha1.EMQXNode{}
	if err := json.Unmarshal(body, &nodeStatuses); err != nil {
		return nil, emperror.Wrap(err, "failed to unmarshal node statuses")
	}
	return nodeStatuses, nil
}

type emqxGateway struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type emqxListener struct {
	Enable bool   `json:"enable"`
	ID     string `json:"id"`
	Bind   string `json:"bind"`
	Type   string `json:"type"`
}

func (r *requestAPI) getAllListenersByAPI(obj client.Object) ([]corev1.ServicePort, error) {
	ports, err := r.getListenerPortsByAPI(obj, "api/v5/listeners")
	if err != nil {
		return nil, err
	}

	gateways, err := r.getGatewaysByAPI(obj)
	if err != nil {
		return nil, err
	}

	for _, gateway := range gateways {
		if strings.ToLower(gateway.Status) == "running" {
			apiPath := fmt.Sprintf("api/v5/gateway/%s/listeners", gateway.Name)
			gatewayPorts, err := r.getListenerPortsByAPI(obj, apiPath)
			if err != nil {
				return nil, err
			}
			ports = append(ports, gatewayPorts...)
		}
	}

	return ports, nil
}

func (r *requestAPI) getGatewaysByAPI(obj client.Object) ([]emqxGateway, error) {
	resp, body, err := r.requestAPI(obj, "GET", "api/v5/gateway", nil)
	if err != nil {
		return nil, emperror.Wrap(err, "failed to get API api/v5/gateway")
	}
	if resp.StatusCode != 200 {
		return nil, emperror.Errorf("failed to get API %s, status : %s, body: %s", "api/v5/gateway", resp.Status, body)
	}
	gateway := []emqxGateway{}
	if err := json.Unmarshal(body, &gateway); err != nil {
		return nil, emperror.Wrap(err, "failed to parse gateway")
	}
	return gateway, nil
}

func (r *requestAPI) getListenerPortsByAPI(obj client.Object, apiPath string) ([]corev1.ServicePort, error) {
	resp, body, err := r.requestAPI(obj, "GET", apiPath, nil)
	if err != nil {
		return nil, emperror.Wrapf(err, "failed to get API %s", apiPath)
	}
	if resp.StatusCode != 200 {
		return nil, emperror.Errorf("failed to get API %s, status : %s, body: %s", apiPath, resp.Status, body)
	}
	ports := []corev1.ServicePort{}
	listeners := []emqxListener{}
	if err := json.Unmarshal(body, &listeners); err != nil {
		return nil, emperror.Wrap(err, "failed to parse listeners")
	}
	for _, listener := range listeners {
		if !listener.Enable {
			continue
		}

		var protocol corev1.Protocol
		compile := regexp.MustCompile(".*(udp|dtls|quic).*")
		if compile.MatchString(listener.Type) {
			protocol = corev1.ProtocolUDP
		} else {
			protocol = corev1.ProtocolTCP
		}

		_, strPort, _ := net.SplitHostPort(listener.Bind)
		intPort, _ := strconv.Atoi(strPort)

		ports = append(ports, corev1.ServicePort{
			Name:       strings.ReplaceAll(listener.ID, ":", "-"),
			Protocol:   protocol,
			Port:       int32(intPort),
			TargetPort: intstr.FromInt(intPort),
		})
	}
	return ports, nil
}
