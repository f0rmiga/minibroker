/*
Copyright 2020 The Kubernetes Authors.

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

package minibroker

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/kubernetes-sigs/minibroker/pkg/helm"
	"github.com/pkg/errors"
	osb "github.com/pmorie/go-open-service-broker-client/v2"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"
)

const (
	InstanceLabel       = "minibroker.instance"
	ServiceKey          = "service-id"
	PlanKey             = "plan-id"
	ProvisionParamsKey  = "provision-params"
	ReleaseNamespaceKey = "release-namespace"
	HeritageLabel       = "heritage"
	ReleaseLabel        = "release"
)

// ConfigMap keys for tracking the last operation
const (
	OperationNameKey        = "last-operation-name"
	OperationStateKey       = "last-operation-state"
	OperationDescriptionKey = "last-operation-description"
)

// Error code constants missing from go-open-service-broker-client
// See https://github.com/pmorie/go-open-service-broker-client/pull/136
const (
	ConcurrencyErrorMessage     = "ConcurrencyError"
	ConcurrencyErrorDescription = "Concurrent modification not supported"
)

// Last operation name prefixes for various operations
const (
	OperationPrefixProvision   = "provision-"
	OperationPrefixDeprovision = "deprovision-"
	OperationPrefixBind        = "bind-"
)

const (
	BindingKeyPrefix      = "binding-"
	BindingStateKeyPrefix = "binding-state-"
)

type Client struct {
	helm                      *helm.Client
	namespace                 string
	coreClient                kubernetes.Interface
	dynamicClient             dynamic.Interface
	providers                 map[string]Provider
	serviceCatalogEnabledOnly bool
}

func NewClient(
	namespace string,
	serviceCatalogEnabledOnly bool,
	clusterDomain string,
) *Client {
	klog.V(5).Infof("minibroker: initializing a new client")
	hb := hostBuilder{clusterDomain}

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}

	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	return &Client{
		helm:                      helm.NewDefaultClient(),
		coreClient:                coreClient,
		dynamicClient:             dynamicClient,
		namespace:                 namespace,
		serviceCatalogEnabledOnly: serviceCatalogEnabledOnly,
		providers: map[string]Provider{
			"mysql":      MySQLProvider{hb},
			"mariadb":    MariadbProvider{hb},
			"postgresql": PostgresProvider{hb},
			"mongodb":    MongodbProvider{hb},
			"redis":      RedisProvider{hb},
			"rabbitmq":   RabbitmqProvider{hb},
		},
	}
}

func (c *Client) Init(repoURL string) error {
	return c.helm.Initialize(repoURL)
}

func hasTag(tag string, list []string) bool {
	for _, listTag := range list {
		if listTag == tag {
			return true
		}
	}

	return false
}

func getTagIntersection(chartVersions repo.ChartVersions) []string {
	tagList := make([][]string, 0)

	for _, chartVersion := range chartVersions {
		tagList = append(tagList, chartVersion.Metadata.Keywords)
	}

	if len(tagList) == 0 {
		return []string{}
	}

	intersection := make([]string, 0)

	// There's only one chart version, so just return its tags
	if len(tagList) == 1 {
		for _, tag := range tagList[0] {
			intersection = append(intersection, tag)
		}

		return intersection
	}

Search:
	for _, searchTag := range tagList[0] {
		for _, other := range tagList[1:] {
			if !hasTag(searchTag, other) {
				// Stop searching for that tag if it isn't found in one of the charts
				continue Search
			}
		}

		// The tag has been found in all of the other keyword lists, so add it
		intersection = append(intersection, searchTag)
	}

	return intersection
}

func generateOperationName(prefix string) string {
	return fmt.Sprintf("%s%x", prefix, rand.Int31())
}

func (c *Client) getConfigMap(instanceID string) (*corev1.ConfigMap, error) {
	configMapInterface := c.coreClient.CoreV1().ConfigMaps(c.namespace)
	config, err := configMapInterface.Get(context.TODO(), instanceID, metav1.GetOptions{})
	if err != nil {
		// Do not wrap the error to keep apierrors.IsNotFound() working correctly
		return nil, err
	}
	return config, nil
}

// updateConfigMap will update the config map data for the given instance; it is
// expected that the config map already exists.
// Each value in data may be either a string (in which case it is set), or nil
// (in which case it is removed); any other value will panic.
func (c *Client) updateConfigMap(instanceID string, data map[string]interface{}) error {
	config, err := c.getConfigMap(instanceID)
	if err != nil {
		return err
	}
	for name, value := range data {
		if value == nil {
			delete(config.Data, name)
		} else if stringValue, ok := value.(string); ok {
			config.Data[name] = stringValue
		} else {
			panic(fmt.Sprintf("Invalid data (key %s), has value %+v", name, value))
		}
	}

	configMapInterface := c.coreClient.CoreV1().ConfigMaps(c.namespace)
	_, err = configMapInterface.Update(context.TODO(), config, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrapf(err, "Failed to update config for instance %q", instanceID)
	}
	return nil
}

func (c *Client) ListServices() ([]osb.Service, error) {
	klog.V(4).Infof("minibroker: listing services")

	var services []osb.Service

	charts := c.helm.ListCharts()
	for chart, chartVersions := range charts {
		if _, ok := c.providers[chart]; !ok && c.serviceCatalogEnabledOnly {
			continue
		}

		tags := getTagIntersection(chartVersions)

		svc := osb.Service{
			ID:          chart,
			Name:        chart,
			Description: "Helm Chart for " + chart,
			Bindable:    true,
			Plans:       make([]osb.Plan, 0, len(chartVersions)),
			Tags:        tags,
		}
		appVersions := map[string]*repo.ChartVersion{}
		for _, chartVersion := range chartVersions {
			if chartVersion.AppVersion == "" {
				continue
			}

			curV, err := semver.NewVersion(chartVersion.Version)
			if err != nil {
				klog.V(4).Infof("minibroker: skipping %s@%s because %q is not a valid semver", chart, chartVersion.AppVersion, chartVersion.Version)
				continue
			}

			currentMax, ok := appVersions[chartVersion.AppVersion]
			if !ok {
				appVersions[chartVersion.AppVersion] = chartVersion
			} else {
				maxV, _ := semver.NewVersion(currentMax.Version)
				if curV.GreaterThan(maxV) {
					appVersions[chartVersion.AppVersion] = chartVersion
				} else {
					klog.V(4).Infof("minibroker: skipping %s@%s because %s < %s", chart, chartVersion.AppVersion, curV, maxV)
					continue
				}
			}
		}

		for _, chartVersion := range appVersions {
			planToken := fmt.Sprintf("%s@%s", chart, chartVersion.AppVersion)
			cleaner := regexp.MustCompile(`[^a-z0-9]`)
			planID := cleaner.ReplaceAllString(strings.ToLower(planToken), "-")
			planName := cleaner.ReplaceAllString(chartVersion.AppVersion, "-")
			plan := osb.Plan{
				ID:          planID,
				Name:        planName,
				Description: chartVersion.Description,
				Free:        boolPtr(true),
			}
			svc.Plans = append(svc.Plans, plan)
		}

		if len(svc.Plans) == 0 {
			continue
		}
		services = append(services, svc)
	}

	klog.V(4).Infof("minibroker: listed services")

	return services, nil
}

// Provision a new service instance.  Returns the async operation key (if
// acceptsIncomplete is set).
func (c *Client) Provision(instanceID, serviceID, planID, namespace string, acceptsIncomplete bool, provisionParams *ProvisionParams) (string, error) {
	klog.V(3).Infof("minibroker: provisioning intance %q, service %q, namespace %q, params %v", instanceID, serviceID, namespace, provisionParams)
	ctx := context.TODO()

	chartName := serviceID
	// The way I'm turning charts into plans is not reversible
	chartVersion := strings.Replace(planID, serviceID+"-", "", 1)
	chartVersion = strings.Replace(chartVersion, "-", ".", -1)

	klog.V(4).Infof("minibroker: persisting the provisioning parameters")
	paramsJSON, err := json.Marshal(provisionParams)
	if err != nil {
		return "", errors.Wrapf(err, "could not marshall provisioning parameters %v", provisionParams)
	}
	config := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceID,
			Namespace: c.namespace,
			Labels: map[string]string{
				ServiceKey: serviceID,
				PlanKey:    planID,
			},
		},
		Data: map[string]string{
			ProvisionParamsKey: string(paramsJSON),
			ServiceKey:         serviceID,
			PlanKey:            planID,
		},
	}

	_, err = c.coreClient.CoreV1().
		ConfigMaps(config.Namespace).
		Create(ctx, &config, metav1.CreateOptions{})
	if err != nil {
		// TODO: compare provision parameters and ignore this call if it's the same
		if apierrors.IsAlreadyExists(err) {
			return "", osb.HTTPStatusCodeError{
				StatusCode:   http.StatusConflict,
				ErrorMessage: &[]string{ConcurrencyErrorMessage}[0],
				Description:  &[]string{ConcurrencyErrorDescription}[0],
			}
		}
		return "", errors.Wrapf(err, "could not persist the instance configmap for %q", instanceID)
	}

	if acceptsIncomplete {
		operationKey := generateOperationName(OperationPrefixProvision)
		err = c.updateConfigMap(instanceID, map[string]interface{}{
			OperationStateKey:       string(osb.StateInProgress),
			OperationNameKey:        operationKey,
			OperationDescriptionKey: fmt.Sprintf("provisioning service instance %q", instanceID),
		})
		if err != nil {
			return "", errors.Wrapf(err, "Failed to set operation key when provisioning instance %q", instanceID)
		}
		go func() {
			err = c.provisionSynchronously(ctx, instanceID, namespace, serviceID, planID, chartName, chartVersion, provisionParams)
			if err == nil {
				err = c.updateConfigMap(instanceID, map[string]interface{}{
					OperationStateKey:       string(osb.StateSucceeded),
					OperationDescriptionKey: fmt.Sprintf("service instance %q provisioned", instanceID),
				})
			} else {
				klog.V(2).Infof("minibroker: failed to provision %q: %v", instanceID, err)
				err = c.updateConfigMap(instanceID, map[string]interface{}{
					OperationStateKey:       string(osb.StateFailed),
					OperationDescriptionKey: fmt.Sprintf("service instance %q failed to provision", instanceID),
				})
				if err != nil {
					klog.V(2).Infof("minibroker: failed to provision %q: could not update operation state when provisioning asynchronously: %v", instanceID, err)
				}
			}
		}()
		return operationKey, nil
	}

	err = c.provisionSynchronously(ctx, instanceID, namespace, serviceID, planID, chartName, chartVersion, provisionParams)
	if err != nil {
		return "", err
	}

	return "", nil
}

// provisionSynchronously will provision the service instance synchronously.
func (c *Client) provisionSynchronously(ctx context.Context, instanceID, namespace, serviceID, planID, chartName, chartVersion string, provisionParams *ProvisionParams) error {
	klog.V(3).Infof("minibroker: provisioning %s/%s using helm chart %s@%s", serviceID, planID, chartName, chartVersion)

	chartDef, err := c.helm.GetChart(chartName, chartVersion)
	if err != nil {
		return err
	}

	release, err := c.helm.ChartClient().Install(chartDef, namespace, provisionParams.Object)
	if err != nil {
		return err
	}

	// Store any required metadata necessary for bind and deprovision as labels on the resources itself
	klog.V(3).Infof("minibroker: labeling chart resources with instance %q", instanceID)
	resources, err := c.helm.ChartClient().ListResources(release)
	if err != nil {
		return err
	}

	for _, r := range resources {
		obj, ok := r.Object.DeepCopyObject().(metav1.Object)
		if !ok {
			continue
		}

		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{InstanceLabel: instanceID}
		} else {
			labels[InstanceLabel] = instanceID
		}
		obj.SetLabels(labels)

		data, err := json.Marshal(obj)
		if err != nil {
			return err
		}

		var dr dynamic.ResourceInterface
		if r.Mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			// Namespaced resources.
			dr = c.dynamicClient.Resource(r.Mapping.Resource).Namespace(obj.GetNamespace())
		} else {
			// Cluster-wide resources.
			dr = c.dynamicClient.Resource(r.Mapping.Resource)
		}

		_, err = dr.Patch(
			ctx,
			obj.GetName(),
			types.StrategicMergePatchType,
			data,
			metav1.PatchOptions{},
		)
		if err != nil {
			return errors.Wrapf(err, "failed to label %s with %s = %s", r.ObjectName(), InstanceLabel, instanceID)
		}
	}

	err = c.updateConfigMap(instanceID, map[string]interface{}{
		ReleaseLabel:        release.Name,
		ReleaseNamespaceKey: release.Namespace,
	})
	if err != nil {
		return errors.Wrapf(err, "could not update the instance configmap for %q", instanceID)
	}

	klog.V(4).Infof("minibroker: provisioned %v@%v (%v@%v)",
		chartName, chartVersion, release.Name, release.Version)

	return nil
}

// Bind the given service instance (of the given service) asynchronously; the
// binding operation key is returned.
func (c *Client) Bind(instanceID, serviceID, bindingID string, acceptsIncomplete bool, bindParams *BindParams) (string, error) {
	klog.V(3).Infof("minibroker: binding instance %q, service %q, binding %q, binding params %v", instanceID, serviceID, bindingID, bindParams)
	config, err := c.getConfigMap(instanceID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("could not find configmap %s/%s", c.namespace, instanceID)
			return "", osb.HTTPStatusCodeError{
				StatusCode:   http.StatusNotFound,
				ErrorMessage: &msg,
			}
		}
		return "", err
	}
	releaseNamespace := config.Data[ReleaseNamespaceKey]
	rawProvisionParams := config.Data[ProvisionParamsKey]
	operationName := generateOperationName(OperationPrefixBind)

	var provisionParams *ProvisionParams
	err = json.Unmarshal([]byte(rawProvisionParams), &provisionParams)
	if err != nil {
		return "", errors.Wrapf(err, "could not unmarshall provision parameters for instance %q", instanceID)
	}

	if acceptsIncomplete {
		klog.V(3).Infof("minibroker: initializing asynchronous binding %q", bindingID)
		go func() {
			_ = c.bindSynchronously(
				instanceID,
				serviceID,
				bindingID,
				releaseNamespace,
				bindParams,
				provisionParams,
			)
			klog.V(3).Infof("minibroker: asynchronously bound instance %q, service %q, binding %q", instanceID, serviceID, bindingID)
		}()
		return operationName, nil
	}

	klog.V(3).Infof("minibroker: initializing synchronous binding %q", bindingID)
	if err := c.bindSynchronously(
		instanceID,
		serviceID,
		bindingID,
		releaseNamespace,
		bindParams,
		provisionParams,
	); err != nil {
		return "", err
	}

	klog.V(3).Infof("minibroker: synchronously bound instance %q, service %q, binding %q", instanceID, serviceID, bindingID)

	return "", nil
}

// bindSynchronously creates a new binding for the given service instance.  All
// results are only reported via the service instance configmap (under the
// appropriate key for the binding) for lookup by LastBindingOperationState().
func (c *Client) bindSynchronously(
	instanceID,
	serviceID,
	bindingID,
	releaseNamespace string,
	bindParams *BindParams,
	provisionParams *ProvisionParams,
) error {
	ctx := context.TODO()

	// Wrap most of the code in an inner function to simplify error handling
	err := func() error {
		filterByInstance := metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(map[string]string{
				InstanceLabel: instanceID,
			}).String(),
		}

		services, err := c.coreClient.CoreV1().
			Services(releaseNamespace).
			List(ctx, filterByInstance)
		if err != nil {
			return errors.Wrapf(err, "failed to get services")
		}
		if len(services.Items) == 0 {
			return fmt.Errorf("failed to get services: no services found")
		}

		secrets, err := c.coreClient.CoreV1().
			Secrets(releaseNamespace).
			List(ctx, filterByInstance)
		if err != nil {
			return errors.Wrapf(err, "failed to get secrets")
		}
		if len(secrets.Items) == 0 {
			return fmt.Errorf("failed to get secrets: no secrets found")
		}

		data := make(Object)
		for _, secret := range secrets.Items {
			for key, value := range secret.Data {
				data[key] = string(value)
			}
		}

		// Apply additional provisioning logic for Service Catalog Enabled services
		provider, ok := c.providers[serviceID]
		if ok {
			creds, err := provider.Bind(
				services.Items,
				bindParams,
				provisionParams,
				data,
			)
			if err != nil {
				return errors.Wrapf(err, "unable to bind instance %s", instanceID)
			}
			for k, v := range creds {
				data[k] = v
			}
		}

		// Record the result for later fetching
		bindingResponse := osb.GetBindingResponse{
			Credentials: data,
			Parameters:  bindParams.Object,
		}
		bindingResponseJSON, err := json.Marshal(bindingResponse)
		if err != nil {
			return errors.Wrapf(err, "failed to store binding parameters")
		}

		err = c.updateConfigMap(instanceID, map[string]interface{}{
			(BindingKeyPrefix + bindingID): string(bindingResponseJSON),
		})
		if err != nil {
			return errors.Wrapf(err, "failed to update binding config")
		}

		return nil
	}()

	operationState := osb.LastOperationResponse{}
	if err == nil {
		operationState.State = osb.StateSucceeded
	} else {
		klog.V(2).Infof("minibroker: error binding instance %q: %v", instanceID, err)
		operationState.State = osb.StateFailed
		operationState.Description = strPtr(fmt.Sprintf("Failed to bind instance %q", instanceID))
	}
	operationStateJSON, marshalError := json.Marshal(operationState)
	if marshalError != nil {
		klog.V(2).Infof("minibroker: error serializing bind operation state: %v", marshalError)
		if err != nil {
			return err
		}
		return marshalError
	}
	updates := map[string]interface{}{
		(BindingStateKeyPrefix + bindingID): string(operationStateJSON),
	}
	updateError := c.updateConfigMap(instanceID, updates)
	if updateError != nil {
		klog.V(2).Infof("minibroker: error updating bind status: %v", marshalError)
		if err != nil {
			return err
		}
		return updateError
	}
	return nil
}

// Unbind a previously-bound instance binding.
func (c *Client) Unbind(instanceID, bindingID string) error {
	klog.V(3).Infof("minibroker: unbinding instance %q binding %q", instanceID, bindingID)

	// The only clean up we need to do is to remove the binding information.
	data := map[string]interface{}{
		(BindingStateKeyPrefix + bindingID): nil,
		(BindingKeyPrefix + bindingID):      nil,
	}
	if err := c.updateConfigMap(instanceID, data); err != nil {
		return err
	}

	klog.V(3).Infof("minibroker: unbound instance %q binding %q", instanceID, bindingID)

	return nil
}

func (c *Client) GetBinding(instanceID, bindingID string) (*osb.GetBindingResponse, error) {
	klog.V(3).Infof("minibroker: getting instance %q binding %q", instanceID, bindingID)

	config, err := c.getConfigMap(instanceID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, osb.HTTPStatusCodeError{StatusCode: http.StatusNotFound}
		}
		return nil, errors.Wrapf(err, "failed to get service instance %q data", instanceID)
	}
	jsonData, ok := config.Data[BindingKeyPrefix+bindingID]
	if !ok {
		return nil, osb.HTTPStatusCodeError{StatusCode: http.StatusNotFound}
	}
	var data *osb.GetBindingResponse
	err = json.Unmarshal([]byte(jsonData), &data)
	if err != nil {
		return nil, errors.Wrapf(err, "Could not decode binding data")
	}

	klog.V(3).Infof("minibroker: got instance %q binding %q", instanceID, bindingID)

	return data, nil
}

func (c *Client) Deprovision(instanceID string, acceptsIncomplete bool) (string, error) {
	klog.V(3).Infof("minibroker: deprovisioning instance %q", instanceID)

	ctx := context.TODO()

	config, err := c.coreClient.CoreV1().
		ConfigMaps(c.namespace).
		Get(ctx, instanceID, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", osb.HTTPStatusCodeError{StatusCode: http.StatusGone}
		}
		return "", err
	}
	release := config.Data[ReleaseLabel]
	namespace := config.Data[ReleaseNamespaceKey]

	if !acceptsIncomplete {
		klog.V(3).Infof("minibroker: synchronously deprovisioning instance %q", instanceID)
		if err := c.deprovisionSynchronously(instanceID, release, namespace); err != nil {
			return "", err
		}
		klog.V(3).Infof("minibroker: synchronously deprovisioned instance %q", instanceID)
		return "", nil
	}

	klog.V(3).Infof("minibroker: asynchronously deprovisioning instance %q", instanceID)
	operationKey := generateOperationName(OperationPrefixDeprovision)
	err = c.updateConfigMap(instanceID, map[string]interface{}{
		OperationStateKey:       string(osb.StateInProgress),
		OperationNameKey:        operationKey,
		OperationDescriptionKey: fmt.Sprintf("deprovisioning service instance %q", instanceID),
	})
	if err != nil {
		return "", errors.Wrapf(err, "Failed to set operation key when deprovisioning instance %s", instanceID)
	}
	go func() {
		err = c.deprovisionSynchronously(instanceID, release, namespace)
		if err == nil {
			// After deprovisioning, there is no config map to update
			return
		}
		klog.V(2).Infof("minibroker: failed to deprovision %q: %v", instanceID, err)
		err = c.updateConfigMap(instanceID, map[string]interface{}{
			OperationStateKey:       string(osb.StateFailed),
			OperationDescriptionKey: fmt.Sprintf("service instance %q failed to deprovision", instanceID),
		})
		if err != nil {
			klog.V(2).Infof("minibroker: could not update operation state when deprovisioning asynchronously: %v", err)
		}
		klog.V(3).Infof("minibroker: asynchronously deprovisioned instance %q", instanceID)
	}()
	return operationKey, nil
}

func (c *Client) deprovisionSynchronously(instanceID, releaseName, namespace string) error {
	ctx := context.TODO()

	if err := c.helm.ChartClient().Uninstall(releaseName, namespace); err != nil {
		return errors.Wrapf(err, "could not uninstall release %s", releaseName)
	}

	err := c.coreClient.CoreV1().
		ConfigMaps(c.namespace).
		Delete(ctx, instanceID, metav1.DeleteOptions{})
	if err != nil {
		return errors.Wrapf(err, "could not delete configmap %s/%s", c.namespace, instanceID)
	}

	return nil
}

// LastOperationState returns the status of the last asynchronous operation. TODO(f0rmiga): This
// deserves some polimorphism.
func (c *Client) LastOperationState(instanceID string, operationKey *osb.OperationKey) (*osb.LastOperationResponse, error) {
	ctx := context.TODO()

	if operationKey != nil {
		klog.V(4).Infof("minibroker: getting last operation state for instance %q using key %q", instanceID, *operationKey)
	} else {
		klog.V(4).Infof("minibroker: getting last operation state for instance %q without key", instanceID)
	}

	config, err := c.coreClient.CoreV1().
		ConfigMaps(c.namespace).
		Get(ctx, instanceID, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if operationKey != nil {
				klog.V(5).Infof("minibroker: missing instance %q while getting last operation state using key %q", instanceID, *operationKey)
			} else {
				klog.V(5).Infof("minibroker: missing instance %q while getting last operation state without key", instanceID)
			}
			return nil, osb.HTTPStatusCodeError{
				StatusCode: http.StatusGone,
			}
		}
		return nil, err
	}

	if operationKey != nil && config.Data[OperationNameKey] != string(*operationKey) {
		// Got unexpected operation key.
		if operationKey != nil {
			klog.V(4).Infof("minibroker: failed to get last operation state for instance %q using key %q", instanceID, *operationKey)
		} else {
			klog.V(4).Infof("minibroker: failed to get last operation state for instance %q without key", instanceID)
		}
		return nil, osb.HTTPStatusCodeError{
			StatusCode:   http.StatusBadRequest,
			ErrorMessage: strPtr(ConcurrencyErrorMessage),
			Description:  strPtr(ConcurrencyErrorDescription),
		}
	}

	description := config.Data[OperationDescriptionKey]
	response := &osb.LastOperationResponse{
		State:       osb.LastOperationState(config.Data[OperationStateKey]),
		Description: &description,
	}

	if operationKey != nil {
		klog.V(4).Infof("minibroker: got last operation state for instance %q using key %q", instanceID, *operationKey)
	} else {
		klog.V(4).Infof("minibroker: got last operation state for instance %q without key", instanceID)
	}

	return response, nil
}

func boolPtr(value bool) *bool {
	return &value
}

func strPtr(value string) *string {
	return &value
}

func (c *Client) LastBindingOperationState(instanceID, bindingID string) (*osb.LastOperationResponse, error) {
	klog.V(4).Infof("minibroker: getting last binding %q operation state for instance %q", bindingID, instanceID)
	config, err := c.getConfigMap(instanceID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(5).Infof("minibroker: missing instance %q while getting last binding %q operation state", instanceID, bindingID)
			return nil, osb.HTTPStatusCodeError{
				StatusCode: http.StatusGone,
			}
		}
	}

	stateJSON, ok := config.Data[BindingStateKeyPrefix+bindingID]
	if !ok {
		klog.V(5).Infof("minibroker: missing binding %q for instance %q while getting last binding operation state", bindingID, instanceID)
		return nil, osb.HTTPStatusCodeError{
			StatusCode: http.StatusGone,
		}
	}

	var response *osb.LastOperationResponse
	err = json.Unmarshal([]byte(stateJSON), &response)
	if err != nil {
		return nil, errors.Wrapf(err, "Error unmarshalling binding state %s", stateJSON)
	}

	klog.V(4).Infof("minibroker: got last binding %q operation state for instance %q", bindingID, instanceID)
	return response, nil
}
