/*
Copyright 2018 The Rook Authors. All rights reserved.

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

package mgr

import (
	_ "embed"
	"fmt"
	"os"
	"strconv"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookv1 "github.com/rook/rook/pkg/apis/rook.io/v1"
	"github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	"github.com/rook/rook/pkg/operator/ceph/config"
	"github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	podIPEnvVar              = "ROOK_POD_IP"
	serviceMetricName        = "http-metrics"
	rookMonitoringPrometheus = "ROOK_CEPH_MONITORING_PROMETHEUS_RULE"
)

// Local package template path for prometheusrule
//go:embed template/prometheusrule.yaml
var PrometheusRuleTemplatePath string

//go:embed template/prometheusrule-external.yaml
var PrometheusRuleExternalTemplatePath string

func (c *Cluster) makeDeployment(mgrConfig *mgrConfig) (*apps.Deployment, error) {
	logger.Debugf("mgrConfig: %+v", mgrConfig)
	podSpec := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name:   mgrConfig.ResourceName,
			Labels: c.getPodLabels(mgrConfig.DaemonID, true),
		},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{
				c.makeChownInitContainer(mgrConfig),
			},
			Containers: []v1.Container{
				c.makeMgrDaemonContainer(mgrConfig),
			},
			ServiceAccountName: serviceAccountName,
			RestartPolicy:      v1.RestartPolicyAlways,
			Volumes:            controller.DaemonVolumes(mgrConfig.DataPathMap, mgrConfig.ResourceName),
			HostNetwork:        c.spec.Network.IsHost(),
			PriorityClassName:  cephv1.GetMgrPriorityClassName(c.spec.PriorityClassNames),
		},
	}
	cephv1.GetMgrPlacement(c.spec.Placement).ApplyToPodSpec(&podSpec.Spec)

	// Run the sidecar and require anti affinity only if there are multiple mgrs
	if c.spec.Mgr.Count > 1 {
		podSpec.Spec.Containers = append(podSpec.Spec.Containers, c.makeMgrSidecarContainer(mgrConfig))
		matchLabels := controller.AppLabels(AppName, c.clusterInfo.Namespace)

		// Stretch the mgrs across hosts by default, or across a bigger failure domain for stretch clusters
		topologyKey := v1.LabelHostname
		if c.spec.IsStretchCluster() {
			topologyKey = mon.StretchFailureDomainLabel(c.spec)
		}
		k8sutil.SetNodeAntiAffinityForPod(&podSpec.Spec, !c.spec.Mgr.AllowMultiplePerNode, topologyKey, matchLabels, nil)
	}

	// If the log collector is enabled we add the side-car container
	if c.spec.LogCollector.Enabled {
		shareProcessNamespace := true
		podSpec.Spec.ShareProcessNamespace = &shareProcessNamespace
		podSpec.Spec.Containers = append(podSpec.Spec.Containers, *controller.LogCollectorContainer(fmt.Sprintf("ceph-mgr.%s", mgrConfig.DaemonID), c.clusterInfo.Namespace, c.spec))
	}

	// Replace default unreachable node toleration
	k8sutil.AddUnreachableNodeToleration(&podSpec.Spec)

	if c.spec.Network.IsHost() {
		podSpec.Spec.DNSPolicy = v1.DNSClusterFirstWithHostNet
	} else if c.spec.Network.NetworkSpec.IsMultus() {
		if err := k8sutil.ApplyMultus(c.spec.Network.NetworkSpec, &podSpec.ObjectMeta); err != nil {
			return nil, err
		}
	}

	cephv1.GetMgrAnnotations(c.spec.Annotations).ApplyToObjectMeta(&podSpec.ObjectMeta)
	c.applyPrometheusAnnotations(&podSpec.ObjectMeta)
	cephv1.GetMgrLabels(c.spec.Labels).ApplyToObjectMeta(&podSpec.ObjectMeta)

	replicas := int32(1)

	d := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mgrConfig.ResourceName,
			Namespace: c.clusterInfo.Namespace,
			Labels:    c.getPodLabels(mgrConfig.DaemonID, true),
		},
		Spec: apps.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: c.getPodLabels(mgrConfig.DaemonID, false),
			},
			Template: podSpec,
			Replicas: &replicas,
			Strategy: apps.DeploymentStrategy{
				Type: apps.RecreateDeploymentStrategyType,
			},
		},
	}
	k8sutil.AddRookVersionLabelToDeployment(d)
	cephv1.GetMgrLabels(c.spec.Labels).ApplyToObjectMeta(&d.ObjectMeta)
	controller.AddCephVersionLabelToDeployment(c.clusterInfo.CephVersion, d)
	err := c.clusterInfo.OwnerInfo.SetControllerReference(d)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to set owner reference to mgr deployment %q", d.Name)
	}
	return d, nil
}

func (c *Cluster) makeChownInitContainer(mgrConfig *mgrConfig) v1.Container {
	return controller.ChownCephDataDirsInitContainer(
		*mgrConfig.DataPathMap,
		c.spec.CephVersion.Image,
		controller.DaemonVolumeMounts(mgrConfig.DataPathMap, mgrConfig.ResourceName),
		cephv1.GetMgrResources(c.spec.Resources),
		controller.PodSecurityContext(),
	)
}

func (c *Cluster) makeMgrDaemonContainer(mgrConfig *mgrConfig) v1.Container {

	container := v1.Container{
		Name: "mgr",
		Command: []string{
			"ceph-mgr",
		},
		Args: append(
			controller.DaemonFlags(c.clusterInfo, &c.spec, mgrConfig.DaemonID),
			// for ceph-mgr cephfs
			// see https://github.com/ceph/ceph-csi/issues/486 for more details
			config.NewFlag("client-mount-uid", "0"),
			config.NewFlag("client-mount-gid", "0"),
			"--foreground",
		),
		Image:        c.spec.CephVersion.Image,
		VolumeMounts: controller.DaemonVolumeMounts(mgrConfig.DataPathMap, mgrConfig.ResourceName),
		Ports: []v1.ContainerPort{
			{
				Name:          "mgr",
				ContainerPort: int32(6800),
				Protocol:      v1.ProtocolTCP,
			},
			{
				Name:          "http-metrics",
				ContainerPort: int32(DefaultMetricsPort),
				Protocol:      v1.ProtocolTCP,
			},
			{
				Name:          "dashboard",
				ContainerPort: int32(c.dashboardPort()),
				Protocol:      v1.ProtocolTCP,
			},
		},
		Env: append(
			controller.DaemonEnvVars(c.spec.CephVersion.Image),
			c.cephMgrOrchestratorModuleEnvs()...,
		),
		Resources:       cephv1.GetMgrResources(c.spec.Resources),
		SecurityContext: controller.PodSecurityContext(),
		LivenessProbe:   getDefaultMgrLivenessProbe(),
		WorkingDir:      config.VarLogCephDir,
	}

	// If the liveness probe is enabled
	container = config.ConfigureLivenessProbe(cephv1.KeyMgr, container, c.spec.HealthCheck)

	// If host networking is enabled, we don't need a bind addr that is different from the public addr
	if !c.spec.Network.IsHost() {
		// Opposite of the above, --public-bind-addr will *not* still advertise on the previous
		// port, which makes sense because this is the pod IP, which changes with every new pod.
		container.Args = append(container.Args,
			config.NewFlag("public-addr", controller.ContainerEnvVarReference(podIPEnvVar)))
	}

	return container
}

func (c *Cluster) makeMgrSidecarContainer(mgrConfig *mgrConfig) v1.Container {
	envVars := []v1.EnvVar{
		{Name: "ROOK_CLUSTER_ID", Value: string(c.clusterInfo.OwnerInfo.GetUID())},
		{Name: "ROOK_CLUSTER_NAME", Value: string(c.clusterInfo.NamespacedName().Name)},
		k8sutil.PodIPEnvVar(k8sutil.PrivateIPEnvVar),
		k8sutil.PodIPEnvVar(k8sutil.PublicIPEnvVar),
		mon.PodNamespaceEnvVar(c.clusterInfo.Namespace),
		mon.EndpointEnvVar(),
		mon.SecretEnvVar(),
		mon.CephUsernameEnvVar(),
		mon.CephSecretEnvVar(),
		k8sutil.ConfigOverrideEnvVar(),
		{Name: "ROOK_FSID", ValueFrom: &v1.EnvVarSource{
			SecretKeyRef: &v1.SecretKeySelector{
				LocalObjectReference: v1.LocalObjectReference{Name: "rook-ceph-mon"},
				Key:                  "fsid",
			},
		}},
		{Name: "ROOK_DASHBOARD_ENABLED", Value: strconv.FormatBool(c.spec.Dashboard.Enabled)},
		{Name: "ROOK_MONITORING_ENABLED", Value: strconv.FormatBool(c.spec.Monitoring.Enabled)},
		{Name: "ROOK_UPDATE_INTERVAL", Value: "15s"},
		{Name: "ROOK_DAEMON_NAME", Value: mgrConfig.DaemonID},
		{Name: "ROOK_MGR_STAT_SUPPORTED", Value: strconv.FormatBool(c.clusterInfo.CephVersion.IsAtLeastPacific())},
	}

	return v1.Container{
		Args:      []string{"ceph", "mgr", "watch-active"},
		Name:      "watch-active",
		Image:     c.rookVersion,
		Env:       envVars,
		Resources: cephv1.GetMgrSidecarResources(c.spec.Resources),
	}
}

func getDefaultMgrLivenessProbe() *v1.Probe {
	return &v1.Probe{
		Handler: v1.Handler{
			HTTPGet: &v1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt(int(DefaultMetricsPort)),
			},
		},
		InitialDelaySeconds: 60,
	}
}

// MakeMetricsService generates the Kubernetes service object for the monitoring service
func (c *Cluster) MakeMetricsService(name, activeDaemon, servicePortMetricName string) (*v1.Service, error) {
	labels := c.selectorLabels(activeDaemon)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.clusterInfo.Namespace,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				{
					Name:     servicePortMetricName,
					Port:     int32(DefaultMetricsPort),
					Protocol: v1.ProtocolTCP,
				},
			},
		},
	}

	// If the cluster is external we don't need to add the selector
	if name != controller.ExternalMgrAppName {
		svc.Spec.Selector = labels
	}

	err := c.clusterInfo.OwnerInfo.SetControllerReference(svc)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to set owner reference to monitoring service %q", svc.Name)
	}
	return svc, nil
}

func (c *Cluster) makeDashboardService(name, activeDaemon string) (*v1.Service, error) {
	labels := c.selectorLabels(activeDaemon)

	portName := "https-dashboard"
	if !c.spec.Dashboard.SSL {
		portName = "http-dashboard"
	}
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-dashboard", name),
			Namespace: c.clusterInfo.Namespace,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Selector: labels,
			Type:     v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				{
					Name:     portName,
					Port:     int32(c.dashboardPort()),
					Protocol: v1.ProtocolTCP,
				},
			},
		},
	}
	err := c.clusterInfo.OwnerInfo.SetControllerReference(svc)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to set owner reference to dashboard service %q", svc.Name)
	}
	return svc, nil
}

func (c *Cluster) getPodLabels(daemonName string, includeNewLabels bool) map[string]string {
	labels := controller.CephDaemonAppLabels(AppName, c.clusterInfo.Namespace, "mgr", daemonName, includeNewLabels)
	// leave "instance" key for legacy usage
	labels["instance"] = daemonName
	return labels
}

func (c *Cluster) applyPrometheusAnnotations(objectMeta *metav1.ObjectMeta) {
	if len(cephv1.GetMgrAnnotations(c.spec.Annotations)) == 0 {
		t := rookv1.Annotations{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   strconv.Itoa(int(DefaultMetricsPort)),
		}

		t.ApplyToObjectMeta(objectMeta)
	}
}

func (c *Cluster) cephMgrOrchestratorModuleEnvs() []v1.EnvVar {
	operatorNamespace := os.Getenv(k8sutil.PodNamespaceEnvVar)
	prometheusRules := os.Getenv(rookMonitoringPrometheus)
	envVars := []v1.EnvVar{
		{Name: "ROOK_OPERATOR_NAMESPACE", Value: operatorNamespace},
		{Name: "ROOK_CEPH_CLUSTER_CRD_VERSION", Value: cephv1.Version},
		{Name: "ROOK_CEPH_CLUSTER_CRD_NAME", Value: c.clusterInfo.NamespacedName().Name},
		{Name: rookMonitoringPrometheus, Value: prometheusRules},
		k8sutil.PodIPEnvVar(podIPEnvVar),
	}
	return envVars
}

func (c *Cluster) selectorLabels(activeDaemon string) map[string]string {
	labels := controller.AppLabels(AppName, c.clusterInfo.Namespace)
	if activeDaemon != "" {
		labels[controller.DaemonIDLabel] = activeDaemon
	}
	return labels
}

type PrometheusRuleCustomized struct {
	Labels Labels `yaml:"labels"`
	Alerts Alerts `yaml:"alerts"`
}
type Labels struct {
	Prometheus string `yaml:"prometheus"`
	Role       string `yaml:"role"`
}
type CephMgrIsAbsent struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephMgrIsMissingReplicas struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephMdsMissingReplicas struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephMonQuorumAtRisk struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephMonQuorumLost struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephMonHighNumberOfLeaderChanges struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephNodeDown struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephOSDCriticallyFull struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephOSDFlapping struct {
	Limit         int    `yaml:"limit"`
	OsdUpRate     string `yaml:"osdUpRate"`
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephOSDNearFull struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephOSDDiskNotResponding struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephOSDDiskUnavailable struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephOSDSlowOps struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephDataRecoveryTakingTooLong struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephPGRepairTakingTooLong struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type PersistentVolumeUsageNearFull struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type PersistentVolumeUsageCritical struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephClusterErrorState struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephClusterWarningState struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephOSDVersionMismatch struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephMonVersionMismatch struct {
	For           string `yaml:"for"`
	SeverityLevel string `yaml:"severityLevel"`
	Severity      string `yaml:"severity"`
}
type CephClusterNearFull struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephClusterCriticallyFull struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephClusterReadOnly struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephPoolQuotaBytesNearExhaustion struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type CephPoolQuotaBytesCriticallyExhausted struct {
	Limit         float64 `yaml:"limit"`
	For           string  `yaml:"for"`
	SeverityLevel string  `yaml:"severityLevel"`
	Severity      string  `yaml:"severity"`
}
type Alerts struct {
	CephMgrIsAbsent                       CephMgrIsAbsent                       `yaml:"cephMgrIsAbsent"`
	CephMgrIsMissingReplicas              CephMgrIsMissingReplicas              `yaml:"cephMgrIsMissingReplicas"`
	CephMdsMissingReplicas                CephMdsMissingReplicas                `yaml:"cephMdsMissingReplicas"`
	CephMonQuorumAtRisk                   CephMonQuorumAtRisk                   `yaml:"cephMonQuorumAtRisk"`
	CephMonQuorumLost                     CephMonQuorumLost                     `yaml:"cephMonQuorumLost"`
	CephMonHighNumberOfLeaderChanges      CephMonHighNumberOfLeaderChanges      `yaml:"cephMonHighNumberOfLeaderChanges"`
	CephNodeDown                          CephNodeDown                          `yaml:"cephNodeDown"`
	CephOSDCriticallyFull                 CephOSDCriticallyFull                 `yaml:"cephOSDCriticallyFull"`
	CephOSDFlapping                       CephOSDFlapping                       `yaml:"cephOSDFlapping"`
	CephOSDNearFull                       CephOSDNearFull                       `yaml:"cephOSDNearFull"`
	CephOSDDiskNotResponding              CephOSDDiskNotResponding              `yaml:"cephOSDDiskNotResponding"`
	CephOSDDiskUnavailable                CephOSDDiskUnavailable                `yaml:"cephOSDDiskUnavailable"`
	CephOSDSlowOps                        CephOSDSlowOps                        `yaml:"cephOSDSlowOps"`
	CephDataRecoveryTakingTooLong         CephDataRecoveryTakingTooLong         `yaml:"cephDataRecoveryTakingTooLong"`
	CephPGRepairTakingTooLong             CephPGRepairTakingTooLong             `yaml:"cephPGRepairTakingTooLong"`
	PersistentVolumeUsageNearFull         PersistentVolumeUsageNearFull         `yaml:"PersistentVolumeUsageNearFull"`
	PersistentVolumeUsageCritical         PersistentVolumeUsageCritical         `yaml:"PersistentVolumeUsageCritical"`
	CephClusterErrorState                 CephClusterErrorState                 `yaml:"cephClusterErrorState"`
	CephClusterWarningState               CephClusterWarningState               `yaml:"cephClusterWarningState"`
	CephOSDVersionMismatch                CephOSDVersionMismatch                `yaml:"cephOSDVersionMismatch"`
	CephMonVersionMismatch                CephMonVersionMismatch                `yaml:"cephMonVersionMismatch"`
	CephClusterNearFull                   CephClusterNearFull                   `yaml:"cephClusterNearFull"`
	CephClusterCriticallyFull             CephClusterCriticallyFull             `yaml:"cephClusterCriticallyFull"`
	CephClusterReadOnly                   CephClusterReadOnly                   `yaml:"cephClusterReadOnly"`
	CephPoolQuotaBytesNearExhaustion      CephPoolQuotaBytesNearExhaustion      `yaml:"cephPoolQuotaBytesNearExhaustion"`
	CephPoolQuotaBytesCriticallyExhausted CephPoolQuotaBytesCriticallyExhausted `yaml:"cephPoolQuotaBytesCriticallyExhausted"`
}
