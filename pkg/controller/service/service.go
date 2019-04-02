/*
 * Copyright (c) 2018 WSO2 Inc. (http:www.wso2.org) All Rights Reserved.
 *
 * WSO2 Inc. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http:www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package service

import (
	"fmt"
	"reflect"

	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	appsv1informers "k8s.io/client-go/informers/apps/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/cellery-io/mesh-controller/pkg/apis/mesh/v1alpha1"
	meshclientset "github.com/cellery-io/mesh-controller/pkg/client/clientset/versioned"
	"github.com/cellery-io/mesh-controller/pkg/controller"
	"github.com/cellery-io/mesh-controller/pkg/controller/service/resources"

	//corev1informers "k8s.io/client-go/informers/core/v1"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	autoscalingV2beta1Informer "k8s.io/client-go/informers/autoscaling/v2beta1"
	autoscalingV2beta1Lister "k8s.io/client-go/listers/autoscaling/v2beta1"

	meshinformers "github.com/cellery-io/mesh-controller/pkg/client/informers/externalversions/mesh/v1alpha1"
	listers "github.com/cellery-io/mesh-controller/pkg/client/listers/mesh/v1alpha1"
)

type serviceHandler struct {
	kubeClient            kubernetes.Interface
	meshClient            meshclientset.Interface
	deploymentLister      appsv1listers.DeploymentLister
	hpaLister             autoscalingV2beta1Lister.HorizontalPodAutoscalerLister
	autoscalePolicyLister listers.AutoscalePolicyLister
	k8sServiceLister      corev1listers.ServiceLister
	serviceLister         listers.ServiceLister
	logger                *zap.SugaredLogger
}

func NewController(
	kubeClient kubernetes.Interface,
	meshClient meshclientset.Interface,
	deploymentInformer appsv1informers.DeploymentInformer,
	hpaInformer autoscalingV2beta1Informer.HorizontalPodAutoscalerInformer,
	autoscalePolicyInformer meshinformers.AutoscalePolicyInformer,
	k8sServiceInformer corev1informers.ServiceInformer,
	serviceInformer meshinformers.ServiceInformer,
	logger *zap.SugaredLogger,
) *controller.Controller {
	h := &serviceHandler{
		kubeClient:            kubeClient,
		meshClient:            meshClient,
		deploymentLister:      deploymentInformer.Lister(),
		hpaLister:             hpaInformer.Lister(),
		autoscalePolicyLister: autoscalePolicyInformer.Lister(),
		k8sServiceLister:      k8sServiceInformer.Lister(),
		serviceLister:         serviceInformer.Lister(),
		logger:                logger.Named("service-controller"),
	}
	c := controller.New(h, h.logger, "Service")

	h.logger.Info("Setting up event handlers")
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.Enqueue,
		UpdateFunc: func(old, new interface{}) {
			h.logger.Debugw("Informer update", "old", old, "new", new)
			c.Enqueue(new)
		},
		DeleteFunc: c.Enqueue,
	})
	return c
}

func (h *serviceHandler) Handle(key string) error {
	h.logger.Infof("Handle called with %s", key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		h.logger.Errorf("invalid resource key: %s", key)
		return nil
	}
	serviceOriginal, err := h.serviceLister.Services(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("service '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}
	h.logger.Debugw("lister instance", key, serviceOriginal)
	service := serviceOriginal.DeepCopy()

	if err = h.handle(service); err != nil {
		return err
	}

	if _, err = h.updateStatus(service); err != nil {
		return err
	}

	return nil
}

func (h *serviceHandler) handle(service *v1alpha1.Service) error {

	if err := h.handleDeployment(service); err != nil {
		return err
	}

	if err := h.handleAutoscalePolicy(service); err != nil {
		return err
	}

	if err := h.handleK8sService(service); err != nil {
		return err
	}

	h.updateOwnerCell(service)
	return nil
}

func (h *serviceHandler) handleDeployment(service *v1alpha1.Service) error {
	deployment, err := h.deploymentLister.Deployments(service.Namespace).Get(resources.ServiceDeploymentName(service))
	if errors.IsNotFound(err) {
		deployment, err = h.kubeClient.AppsV1().Deployments(service.Namespace).Create(resources.CreateServiceDeployment(service))
		if err != nil {
			h.logger.Errorf("Failed to create Service Deployment %v", err)
			return err
		}
		h.logger.Debugw("Deployment created", resources.ServiceDeploymentName(service), deployment)
	} else if err != nil {
		return err
	}

	service.Status.AvailableReplicas = deployment.Status.AvailableReplicas

	return nil
}

func (h *serviceHandler) handleAutoscalePolicy(service *v1alpha1.Service) error {
	// temporarily remove creating autoscaling policies
	if service.Spec.Autoscaling == nil {
		h.logger.Debugw("No autoscaling policies defined in Service ", service.Name)
		return nil
	}

	autoscalePolicy, err := h.autoscalePolicyLister.AutoscalePolicies(service.Namespace).Get(resources.ServiceAutoscalePolicyName(service))
	if errors.IsNotFound(err) {
		if service.Spec.Autoscaling != nil {
			autoscalePolicy = resources.CreateAutoscalePolicy(service)
		} else {
			autoscalePolicy = resources.CreateDefaultAutoscalePolicy(service)
		}
		autoscalePolicy, err = h.meshClient.MeshV1alpha1().AutoscalePolicies(service.Namespace).Create(autoscalePolicy)
		if err != nil {
			h.logger.Errorf("Failed to create Autoscale policy %v", err)
			return err
		}
		h.logger.Infow("Autoscale policy created", resources.ServiceAutoscalePolicyName(service), autoscalePolicy)
		autoscalePolicy.Status = "Ready"
		h.updateAutoscalePolicyStatus(autoscalePolicy)

	} else if err != nil {
		return err
	}
	return nil
}

func (h *serviceHandler) updateAutoscalePolicyStatus(policy *v1alpha1.AutoscalePolicy) error {
	_, err := h.meshClient.MeshV1alpha1().AutoscalePolicies(policy.Namespace).Update(policy)
	if err != nil {
		return err
	}
	return nil
}

func (h *serviceHandler) handleK8sService(service *v1alpha1.Service) error {
	k8sService, err := h.k8sServiceLister.Services(service.Namespace).Get(resources.ServiceK8sServiceName(service))
	if errors.IsNotFound(err) {
		k8sService, err = h.kubeClient.CoreV1().Services(service.Namespace).Create(resources.CreateServiceK8sService(service))
		if err != nil {
			h.logger.Errorf("Failed to create Service service %v", err)
			return err
		}
		h.logger.Debugw("Service created", resources.ServiceK8sServiceName(service), k8sService)
	} else if err != nil {
		return err
	}

	service.Status.HostName = k8sService.Name

	return nil
}

func (h *serviceHandler) updateStatus(service *v1alpha1.Service) (*v1alpha1.Service, error) {
	latestService, err := h.serviceLister.Services(service.Namespace).Get(service.Name)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(latestService.Status, service.Status) {
		latestService.Status = service.Status
		// https://github.com/kubernetes/kubernetes/issues/54672
		// return h.meshClient.MeshV1alpha1().Services(service.Namespace).UpdateStatus(latestService)
		return h.meshClient.MeshV1alpha1().Services(service.Namespace).Update(latestService)
	}
	return nil, nil
}

func (h *serviceHandler) updateOwnerCell(service *v1alpha1.Service) {
	if service.Status.OwnerCell == "" {
		ownerRef := metav1.GetControllerOf(service)
		if ownerRef != nil {
			service.Status.OwnerCell = ownerRef.Name
		} else {
			service.Status.OwnerCell = "<none>"
		}
	}
}
