/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2017, 2018 Red Hat, Inc.
 *
 */

package virtconfig

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"

	k8sv1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"

	v1 "kubevirt.io/client-go/apis/core/v1"
	"kubevirt.io/client-go/log"
)

const (
	ConfigMapName                     = "kubevirt-config"
	FeatureGatesKey                   = "feature-gates"
	EmulatedMachinesKey               = "emulated-machines"
	MachineTypeKey                    = "machine-type"
	AllowEmulationKey                 = "debug.useEmulation"
	ImagePullPolicyKey                = "dev.imagePullPolicy"
	MigrationsConfigKey               = "migrations"
	CPUModelKey                       = "default-cpu-model"
	CPURequestKey                     = "cpu-request"
	MemoryOvercommitKey               = "memory-overcommit"
	LessPVCSpaceTolerationKey         = "pvc-tolerate-less-space-up-to-percent"
	NodeSelectorsKey                  = "node-selectors"
	NetworkInterfaceKey               = "default-network-interface"
	PermitSlirpInterface              = "permitSlirpInterface"
	PermitBridgeInterfaceOnPodNetwork = "permitBridgeInterfaceOnPodNetwork"
	NodeDrainTaintDefaultKey          = "kubevirt.io/drain"
	SmbiosConfigKey                   = "smbios"
	SELinuxLauncherTypeKey            = "selinuxLauncherType"
	SupportedGuestAgentVersionsKey    = "supported-guest-agent"
	OVMFPathKey                       = "ovmfPath"
	MemBalloonStatsPeriod             = "memBalloonStatsPeriod"
	CPUAllocationRatio                = "cpu-allocation-ratio"
	PermittedHostDevicesKey           = "permittedHostDevices"
)

type ConfigModifiedFn func()

// NewClusterConfig is a wrapper of NewClusterConfigWithCPUArch with default cpuArch.
func NewClusterConfig(configMapInformer cache.SharedIndexInformer,
	crdInformer cache.SharedIndexInformer,
	kubeVirtInformer cache.SharedIndexInformer,
	namespace string) *ClusterConfig {
	return NewClusterConfigWithCPUArch(
		configMapInformer,
		crdInformer,
		kubeVirtInformer,
		namespace,
		runtime.GOARCH,
	)
}

// NewClusterConfigWithCPUArch represents the `kubevirt-config` config map. It can be used to live-update
// values if the config changes. The config update works like this:
// 1. Check if the config exists. If it does not exist, return the default config
// 2. Check if the config got updated. If so, try to parse and return it
// 3. In case of errors or no updates (resource version stays the same), it returns the values from the last good config
func NewClusterConfigWithCPUArch(configMapInformer cache.SharedIndexInformer,
	crdInformer cache.SharedIndexInformer,
	kubeVirtInformer cache.SharedIndexInformer,
	namespace, cpuArch string) *ClusterConfig {

	defaultConfig := defaultClusterConfig(cpuArch)

	c := &ClusterConfig{
		configMapInformer: configMapInformer,
		crdInformer:       crdInformer,
		kubeVirtInformer:  kubeVirtInformer,
		cpuArch:           cpuArch,
		lock:              &sync.Mutex{},
		namespace:         namespace,
		lastValidConfig:   defaultConfig,
		defaultConfig:     defaultConfig,
	}

	c.configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.configAddedDeleted,
		DeleteFunc: c.configAddedDeleted,
		UpdateFunc: c.configUpdated,
	})

	c.crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.crdAddedDeleted,
		DeleteFunc: c.crdAddedDeleted,
		UpdateFunc: c.crdUpdated,
	})

	c.kubeVirtInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.configAddedDeleted,
		UpdateFunc: c.configUpdated,
	})

	return c
}

func (c *ClusterConfig) configAddedDeleted(_ interface{}) {
	go c.GetConfig()
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.configModifiedCallback != nil {
		for _, callback := range c.configModifiedCallback {
			go callback()
		}
	}
}

func (c *ClusterConfig) configUpdated(_, _ interface{}) {
	go c.GetConfig()
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.configModifiedCallback != nil {
		for _, callback := range c.configModifiedCallback {
			go callback()
		}
	}
}

func isDataVolumeCrd(crd *extv1.CustomResourceDefinition) bool {
	if crd.Spec.Names.Kind == "DataVolume" {
		return true
	}

	return false
}

func isDataSourceCrd(crd *extv1.CustomResourceDefinition) bool {
	if crd.Spec.Names.Kind == "DataSource" {
		return true
	}

	return false
}

func (c *ClusterConfig) crdAddedDeleted(obj interface{}) {
	go c.GetConfig()
	crd := obj.(*extv1.CustomResourceDefinition)
	if !isDataVolumeCrd(crd) && !isDataSourceCrd(crd) {
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	if c.configModifiedCallback != nil {
		for _, callback := range c.configModifiedCallback {
			go callback()

		}
	}
}

func (c *ClusterConfig) crdUpdated(_, cur interface{}) {
	c.crdAddedDeleted(cur)
}

func defaultClusterConfig(cpuArch string) *v1.KubeVirtConfiguration {
	parallelOutboundMigrationsPerNodeDefault := ParallelOutboundMigrationsPerNodeDefault
	parallelMigrationsPerClusterDefault := ParallelMigrationsPerClusterDefault
	bandwithPerMigrationDefault := resource.MustParse(BandwithPerMigrationDefault)
	nodeDrainTaintDefaultKey := NodeDrainTaintDefaultKey
	allowAutoConverge := MigrationAllowAutoConverge
	allowPostCopy := MigrationAllowPostCopy
	defaultUnsafeMigrationOverride := DefaultUnsafeMigrationOverride
	progressTimeout := MigrationProgressTimeout
	completionTimeoutPerGiB := MigrationCompletionTimeoutPerGiB
	cpuRequestDefault := resource.MustParse(DefaultCPURequest)
	nodeSelectorsDefault, _ := parseNodeSelectors(DefaultNodeSelectors)
	defaultNetworkInterface := DefaultNetworkInterface
	defaultMemBalloonStatsPeriod := DefaultMemBalloonStatsPeriod
	SmbiosDefaultConfig := &v1.SMBiosConfiguration{
		Family:       SmbiosConfigDefaultFamily,
		Manufacturer: SmbiosConfigDefaultManufacturer,
		Product:      SmbiosConfigDefaultProduct,
	}
	supportedQEMUGuestAgentVersions := strings.Split(strings.TrimRight(SupportedGuestAgentVersions, ","), ",")
	DefaultOVMFPath, DefaultMachineType, emulatedMachinesDefault := getCPUArchSpecificDefault(cpuArch)
	defaultDiskVerification := &v1.DiskVerification{
		MemoryLimit: resource.NewScaledQuantity(DefaultDiskVerificationMemoryLimitMBytes, resource.Mega),
	}

	return &v1.KubeVirtConfiguration{
		ImagePullPolicy: DefaultImagePullPolicy,
		DeveloperConfiguration: &v1.DeveloperConfiguration{
			UseEmulation:           DefaultAllowEmulation,
			MemoryOvercommit:       DefaultMemoryOvercommit,
			LessPVCSpaceToleration: DefaultLessPVCSpaceToleration,
			MinimumReservePVCBytes: DefaultMinimumReservePVCBytes,
			NodeSelectors:          nodeSelectorsDefault,
			CPUAllocationRatio:     DefaultCPUAllocationRatio,
			DiskVerification:       defaultDiskVerification,
			LogVerbosity: &v1.LogVerbosity{
				VirtAPI:        DefaultVirtAPILogVerbosity,
				VirtOperator:   DefaultVirtOperatorLogVerbosity,
				VirtController: DefaultVirtControllerLogVerbosity,
				VirtHandler:    DefaultVirtHandlerLogVerbosity,
				VirtLauncher:   DefaultVirtLauncherLogVerbosity,
			},
		},
		MigrationConfiguration: &v1.MigrationConfiguration{
			ParallelMigrationsPerCluster:      &parallelMigrationsPerClusterDefault,
			ParallelOutboundMigrationsPerNode: &parallelOutboundMigrationsPerNodeDefault,
			NodeDrainTaintKey:                 &nodeDrainTaintDefaultKey,
			BandwidthPerMigration:             &bandwithPerMigrationDefault,
			ProgressTimeout:                   &progressTimeout,
			CompletionTimeoutPerGiB:           &completionTimeoutPerGiB,
			UnsafeMigrationOverride:           &defaultUnsafeMigrationOverride,
			AllowAutoConverge:                 &allowAutoConverge,
			AllowPostCopy:                     &allowPostCopy,
		},
		MachineType:      DefaultMachineType,
		CPURequest:       &cpuRequestDefault,
		EmulatedMachines: emulatedMachinesDefault,
		NetworkConfiguration: &v1.NetworkConfiguration{
			NetworkInterface:                  defaultNetworkInterface,
			PermitSlirpInterface:              pointer.BoolPtr(DefaultPermitSlirpInterface),
			PermitBridgeInterfaceOnPodNetwork: pointer.BoolPtr(DefaultPermitBridgeInterfaceOnPodNetwork),
		},
		SMBIOSConfig:                SmbiosDefaultConfig,
		SELinuxLauncherType:         DefaultSELinuxLauncherType,
		SupportedGuestAgentVersions: supportedQEMUGuestAgentVersions,
		OVMFPath:                    DefaultOVMFPath,
		MemBalloonStatsPeriod:       &defaultMemBalloonStatsPeriod,
		APIConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtAPIQPS,
				Burst: DefaultVirtAPIBurst,
			}}},
		},
		ControllerConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtControllerQPS,
				Burst: DefaultVirtControllerBurst,
			}}},
		},
		HandlerConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtHandlerQPS,
				Burst: DefaultVirtHandlerBurst,
			}}},
		},
		WebhookConfiguration: &v1.ReloadableComponentConfiguration{
			RestClient: &v1.RESTClientConfiguration{RateLimiter: &v1.RateLimiter{TokenBucketRateLimiter: &v1.TokenBucketRateLimiter{
				QPS:   DefaultVirtWebhookClientQPS,
				Burst: DefaultVirtWebhookClientBurst,
			}}},
		},
	}
}

type ClusterConfig struct {
	configMapInformer                cache.SharedIndexInformer
	crdInformer                      cache.SharedIndexInformer
	kubeVirtInformer                 cache.SharedIndexInformer
	namespace                        string
	cpuArch                          string
	lock                             *sync.Mutex
	lastValidConfig                  *v1.KubeVirtConfiguration
	defaultConfig                    *v1.KubeVirtConfiguration
	lastInvalidConfigResourceVersion string
	lastValidConfigResourceVersion   string
	configModifiedCallback           []ConfigModifiedFn
}

func (c *ClusterConfig) SetConfigModifiedCallback(cb ConfigModifiedFn) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.configModifiedCallback = append(c.configModifiedCallback, cb)
	for _, callback := range c.configModifiedCallback {
		go callback()
	}
}

// This struct is for backward compatibility and is deprecated, no new fields should be added
type migrationConfiguration struct {
	NodeDrainTaintKey                 *string            `json:"nodeDrainTaintKey,omitempty"`
	ParallelOutboundMigrationsPerNode *uint32            `json:"parallelOutboundMigrationsPerNode,string,omitempty"`
	ParallelMigrationsPerCluster      *uint32            `json:"parallelMigrationsPerCluster,string,omitempty"`
	AllowAutoConverge                 *bool              `json:"allowAutoConverge,string,omitempty"`
	BandwidthPerMigration             *resource.Quantity `json:"bandwidthPerMigration,omitempty"`
	CompletionTimeoutPerGiB           *int64             `json:"completionTimeoutPerGiB,string,omitempty"`
	ProgressTimeout                   *int64             `json:"progressTimeout,string,omitempty"`
	UnsafeMigrationOverride           *bool              `json:"unsafeMigrationOverride,string,omitempty"`
	AllowPostCopy                     *bool              `json:"allowPostCopy,string,omitempty"`
	DisableTLS                        *bool              `json:"disableTLS,omitempty"`
}

// setConfigFromConfigMap parses the provided config map and updates the provided config.
// Default values in the provided config stay intact.
func setConfigFromConfigMap(config *v1.KubeVirtConfiguration, configMap *k8sv1.ConfigMap) error {
	// set migration options
	rawConfig := strings.TrimSpace(configMap.Data[MigrationsConfigKey])
	if rawConfig != "" {
		migrationConfig := migrationConfiguration(*config.MigrationConfiguration)
		// only sets values if they were specified, default values stay intact
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(rawConfig), 1024).Decode(&migrationConfig)
		if err != nil {
			return fmt.Errorf("failed to parse migration config: %v", err)
		}
		converted := v1.MigrationConfiguration(migrationConfig)
		config.MigrationConfiguration = &converted
	}

	// set smbios values if they exist
	smbiosConfig := strings.TrimSpace(configMap.Data[SmbiosConfigKey])
	if smbiosConfig != "" {
		// only set values if they were specified, default  values stay intact
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(smbiosConfig), 1024).Decode(config.SMBIOSConfig)
		if err != nil {
			return fmt.Errorf("failed to parse SMBIOS config: %v", err)
		}
	}

	// updates host devices in the config.
	var newPermittedHostDevices *v1.PermittedHostDevices
	rawConfig = strings.TrimSpace(configMap.Data[PermittedHostDevicesKey])
	if rawConfig != "" {
		newPermittedHostDevices = &v1.PermittedHostDevices{}
		err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(rawConfig), 1024).Decode(newPermittedHostDevices)
		if err != nil {
			return fmt.Errorf("failed to parse host devices config: %v", err)
		}
	}
	config.PermittedHostDevices = newPermittedHostDevices

	// set image pull policy
	policy := strings.TrimSpace(configMap.Data[ImagePullPolicyKey])
	switch policy {
	case "":
		// keep the default
	case "Always":
		config.ImagePullPolicy = k8sv1.PullAlways
	case "Never":
		config.ImagePullPolicy = k8sv1.PullNever
	case "IfNotPresent":
		config.ImagePullPolicy = k8sv1.PullIfNotPresent
	default:
		return fmt.Errorf("invalid dev.imagePullPolicy in config: %v", policy)
	}

	// set if emulation is used
	allowEmulation := strings.TrimSpace(configMap.Data[AllowEmulationKey])
	switch allowEmulation {
	case "":
		// keep the default
	case "true":
		config.DeveloperConfiguration.UseEmulation = true
	case "false":
		config.DeveloperConfiguration.UseEmulation = false
	default:
		return fmt.Errorf("invalid %s in config: %v", AllowEmulationKey, allowEmulation)
	}

	// set machine type
	if machineType := strings.TrimSpace(configMap.Data[MachineTypeKey]); machineType != "" {
		config.MachineType = machineType
	}

	if cpuModel := strings.TrimSpace(configMap.Data[CPUModelKey]); cpuModel != "" {
		config.CPUModel = cpuModel
	}

	if cpuRequest := strings.TrimSpace(configMap.Data[CPURequestKey]); cpuRequest != "" {
		*config.CPURequest = resource.MustParse(cpuRequest)
	}

	if memoryOvercommit := strings.TrimSpace(configMap.Data[MemoryOvercommitKey]); memoryOvercommit != "" {
		if value, err := strconv.Atoi(memoryOvercommit); err == nil && value > 0 {
			config.DeveloperConfiguration.MemoryOvercommit = value
		} else {
			return fmt.Errorf("Invalid memoryOvercommit in ConfigMap: %s", memoryOvercommit)
		}
	}

	if cpuOvercommit := strings.TrimSpace(configMap.Data[CPUAllocationRatio]); cpuOvercommit != "" {
		if value, err := strconv.ParseInt(cpuOvercommit, 10, 32); err == nil && value > 0 {
			config.DeveloperConfiguration.CPUAllocationRatio = int(value)
		} else {
			return fmt.Errorf("Invalid cpu allocation ratio in ConfigMap: %s", cpuOvercommit)
		}
	}

	if emulatedMachines := strings.TrimSpace(configMap.Data[EmulatedMachinesKey]); emulatedMachines != "" {
		config.EmulatedMachines = stringToStringArray(emulatedMachines)
	}

	if featureGates := strings.TrimSpace(configMap.Data[FeatureGatesKey]); featureGates != "" {
		config.DeveloperConfiguration.FeatureGates = stringToStringArray(featureGates)
	}

	if toleration := strings.TrimSpace(configMap.Data[LessPVCSpaceTolerationKey]); toleration != "" {
		if value, err := strconv.Atoi(toleration); err != nil || value < 0 || value > 100 {
			return fmt.Errorf("Invalid lessPVCSpaceToleration in ConfigMap: %s", toleration)
		} else {
			config.DeveloperConfiguration.LessPVCSpaceToleration = value
		}
	}

	if nodeSelectors := strings.TrimSpace(configMap.Data[NodeSelectorsKey]); nodeSelectors != "" {
		if selectors, err := parseNodeSelectors(nodeSelectors); err != nil {
			return err
		} else {
			config.DeveloperConfiguration.NodeSelectors = selectors
		}
	}

	// disable slirp
	permitSlirp := strings.TrimSpace(configMap.Data[PermitSlirpInterface])
	switch permitSlirp {
	case "":
		// keep the default
	case "true":
		config.NetworkConfiguration.PermitSlirpInterface = pointer.BoolPtr(true)
	case "false":
		config.NetworkConfiguration.PermitSlirpInterface = pointer.BoolPtr(false)
	default:
		return fmt.Errorf("invalid value for permitSlirpInterfaces in config: %v", permitSlirp)
	}

	// disable bridge
	permitBridge := strings.TrimSpace(configMap.Data[PermitBridgeInterfaceOnPodNetwork])
	switch permitBridge {
	case "":
		// keep the default
	case "false":
		config.NetworkConfiguration.PermitBridgeInterfaceOnPodNetwork = pointer.BoolPtr(false)
	case "true":
		config.NetworkConfiguration.PermitBridgeInterfaceOnPodNetwork = pointer.BoolPtr(true)
	default:
		return fmt.Errorf("invalid value for permitBridgeInterfaceOnPodNetwork in config: %v", permitBridge)
	}

	// set default network interface
	iface := strings.TrimSpace(configMap.Data[NetworkInterfaceKey])
	switch iface {
	case "":
		// keep the default
	case string(v1.BridgeInterface), string(v1.SlirpInterface), string(v1.MasqueradeInterface):
		config.NetworkConfiguration.NetworkInterface = iface
	default:
		return fmt.Errorf("invalid default-network-interface in config: %v", iface)
	}

	if selinuxLauncherType := strings.TrimSpace(configMap.Data[SELinuxLauncherTypeKey]); selinuxLauncherType != "" {
		config.SELinuxLauncherType = selinuxLauncherType
	}

	if supportedGuestAgentVersions := strings.TrimSpace(configMap.Data[SupportedGuestAgentVersionsKey]); supportedGuestAgentVersions != "" {
		vals := strings.Split(strings.TrimRight(supportedGuestAgentVersions, ","), ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		config.SupportedGuestAgentVersions = vals
	}

	if ovmfPath := strings.TrimSpace(configMap.Data[OVMFPathKey]); ovmfPath != "" {
		config.OVMFPath = ovmfPath
	}

	if memBalloonStatsPeriod := strings.TrimSpace(configMap.Data[MemBalloonStatsPeriod]); memBalloonStatsPeriod != "" {
		i, err := strconv.Atoi(memBalloonStatsPeriod)
		if err != nil {
			return fmt.Errorf("invalid memBalloonStatsPeriod in config, %s", memBalloonStatsPeriod)
		}
		if i >= 0 {
			mem := uint32(i)
			config.MemBalloonStatsPeriod = &mem
		} else {
			return fmt.Errorf("invalid memBalloonStatsPeriod (negative) in config, %d", i)
		}
	}

	return nil
}

func setConfigFromKubeVirt(config *v1.KubeVirtConfiguration, kv *v1.KubeVirt) error {
	kvConfig := &kv.Spec.Configuration
	overrides, err := json.Marshal(kvConfig)
	if err != nil {
		return err
	}

	err = json.Unmarshal(overrides, &config)
	if err != nil {
		return err
	}

	return nil
}

// getCPUArchSpecificDefault get arch specific default config
func getCPUArchSpecificDefault(cpuArch string) (string, string, []string) {
	// get arch specific default config
	switch cpuArch {
	case "arm64":
		emulatedMachinesDefault := strings.Split(DefaultAARCH64EmulatedMachines, ",")
		return DefaultAARCH64OVMFPath, DefaultAARCH64MachineType, emulatedMachinesDefault
	case "ppc64le":
		emulatedMachinesDefault := strings.Split(DefaultPPC64LEEmulatedMachines, ",")
		return DefaultARCHOVMFPath, DefaultPPC64LEMachineType, emulatedMachinesDefault
	default:
		emulatedMachinesDefault := strings.Split(DefaultAMD64EmulatedMachines, ",")
		return DefaultARCHOVMFPath, DefaultAMD64MachineType, emulatedMachinesDefault
	}
}

// getConfig returns the latest valid parsed config map result, or updates it
// if a newer version is available.
// XXX Rework this, to happen mostly in informer callbacks.
// This will also allow us then to react to config changes and e.g. restart some controllers
func (c *ClusterConfig) GetConfig() (config *v1.KubeVirtConfiguration) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var configMap *k8sv1.ConfigMap
	var kv *v1.KubeVirt
	var resourceVersion string
	var resourceType string
	useConfigMap := false

	if obj, exists, err := c.configMapInformer.GetStore().GetByKey(c.namespace + "/" + ConfigMapName); err != nil {
		log.DefaultLogger().Reason(err).Errorf("Error loading the cluster config from ConfigMap cache, falling back to last good resource version '%s'", c.lastValidConfigResourceVersion)
		return c.lastValidConfig
	} else if !exists {
		kv = c.GetConfigFromKubeVirtCR()
		if kv == nil {
			return c.lastValidConfig
		}

		resourceType = "KubeVirt"
		resourceVersion = kv.ResourceVersion
	} else {
		useConfigMap = true
		resourceType = "ConfigMap"
		configMap = obj.(*k8sv1.ConfigMap)
		resourceVersion = configMap.ResourceVersion
	}

	// if there is a configuration config map present we should use its configuration
	// and ignore configuration in kubevirt
	if c.lastValidConfigResourceVersion == resourceVersion ||
		c.lastInvalidConfigResourceVersion == resourceVersion {
		return c.lastValidConfig
	}

	config = defaultClusterConfig(c.cpuArch)
	var err error
	if useConfigMap {
		err = setConfigFromConfigMap(config, configMap)
	} else {
		err = setConfigFromKubeVirt(config, kv)
	}

	if err != nil {
		c.lastInvalidConfigResourceVersion = resourceVersion
		log.DefaultLogger().Reason(err).Errorf("Invalid cluster config using '%s' resource version '%s', falling back to last good resource version '%s'", resourceType, resourceVersion, c.lastValidConfigResourceVersion)
		return c.lastValidConfig
	}

	log.DefaultLogger().Infof("Updating cluster config from %s to resource version '%s'", resourceType, resourceVersion)
	c.lastValidConfigResourceVersion = resourceVersion
	c.lastValidConfig = config
	return c.lastValidConfig
}

func (c *ClusterConfig) GetConfigFromKubeVirtCR() *v1.KubeVirt {
	objects := c.kubeVirtInformer.GetStore().List()
	var kubeVirtName string
	for _, obj := range objects {
		if kv, ok := obj.(*v1.KubeVirt); ok && kv.DeletionTimestamp == nil {
			if kv.Status.Phase != "" {
				kubeVirtName = kv.Name
			}
		}
	}

	if kubeVirtName == "" {
		return nil
	}

	if obj, exists, err := c.kubeVirtInformer.GetStore().GetByKey(c.namespace + "/" + kubeVirtName); err != nil {
		log.DefaultLogger().Reason(err).Errorf("Error loading the cluster config from KubeVirt cache, falling back to last good resource version '%s'", c.lastValidConfigResourceVersion)
		return nil
	} else if !exists {
		// this path should not be possible
		return nil
	} else {
		return obj.(*v1.KubeVirt)
	}
}

func (c *ClusterConfig) HasDataSourceAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdInformer.GetStore().List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isDataSourceCrd(crd) {
				return true
			}
		}
	}
	return false
}

func (c *ClusterConfig) HasDataVolumeAPI() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	objects := c.crdInformer.GetStore().List()
	for _, obj := range objects {
		if crd, ok := obj.(*extv1.CustomResourceDefinition); ok && crd.DeletionTimestamp == nil {
			if isDataVolumeCrd(crd) {
				return true
			}
		}
	}
	return false
}

func parseNodeSelectors(str string) (map[string]string, error) {
	nodeSelectors := make(map[string]string)
	for _, s := range strings.Split(strings.TrimSpace(str), "\n") {
		v := strings.Split(s, "=")
		if len(v) != 2 {
			return nil, fmt.Errorf("Invalid node selector: %s", s)
		}
		nodeSelectors[v[0]] = v[1]
	}
	return nodeSelectors, nil
}

func stringToStringArray(str string) []string {
	vals := strings.Split(str, ",")
	for i := range vals {
		vals[i] = strings.TrimSpace(vals[i])
	}
	return vals
}
