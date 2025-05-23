/*
SPDX-FileCopyrightText: Red Hat

SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pluginv1alpha1 "github.com/openshift-kni/oran-hwmgr-plugin/api/hwmgr-plugin/v1alpha1"
	hwv1alpha1 "github.com/openshift-kni/oran-o2ims/api/hardwaremanagement/v1alpha1"
	provisioningv1alpha1 "github.com/openshift-kni/oran-o2ims/api/provisioning/v1alpha1"
	"github.com/openshift-kni/oran-o2ims/internal/controllers/utils"
)

// createOrUpdateNodePool creates a new NodePool resource if it doesn't exist or updates it if the spec has changed.
func (t *provisioningRequestReconcilerTask) createOrUpdateNodePool(ctx context.Context, nodePool *hwv1alpha1.NodePool) error {

	existingNodePool := &hwv1alpha1.NodePool{}

	exist, err := utils.DoesK8SResourceExist(ctx, t.client, nodePool.Name, nodePool.Namespace, existingNodePool)
	if err != nil {
		return fmt.Errorf("failed to get NodePool %s in namespace %s: %w", nodePool.GetName(), nodePool.GetNamespace(), err)
	}

	if !exist {
		return t.createNodePoolResources(ctx, nodePool)
	}

	// The template validate is already completed; compare NodeGroup and update them if necessary
	if !equality.Semantic.DeepEqual(existingNodePool.Spec.NodeGroup, nodePool.Spec.NodeGroup) {
		// Only process the configuration changes
		patch := client.MergeFrom(existingNodePool.DeepCopy())
		// Update the spec field with the new data
		existingNodePool.Spec = nodePool.Spec
		// Apply the patch to update the NodePool with the new spec
		if err = t.client.Patch(ctx, existingNodePool, patch); err != nil {
			return fmt.Errorf("failed to patch NodePool %s in namespace %s: %w", nodePool.GetName(), nodePool.GetNamespace(), err)
		}

		// Set hardware configuration start time after the NodePool is updated
		if t.object.Status.Extensions.NodePoolRef.HardwareConfiguringCheckStart.IsZero() {
			currentTime := metav1.Now()
			t.object.Status.Extensions.NodePoolRef.HardwareConfiguringCheckStart = &currentTime
		}
		err = utils.UpdateK8sCRStatus(ctx, t.client, t.object)
		if err != nil {
			return fmt.Errorf("failed to update status for ProvisioningRequest %s: %w", t.object.Name, err)
		}

		t.logger.InfoContext(
			ctx,
			fmt.Sprintf(
				"NodePool (%s) in the namespace %s configuration changes have been detected",
				nodePool.GetName(),
				nodePool.GetNamespace(),
			),
		)
	}
	return nil
}

func (t *provisioningRequestReconcilerTask) createNodePoolResources(ctx context.Context, nodePool *hwv1alpha1.NodePool) error {
	// Create the hardware plugin namespace.
	pluginNameSpace := nodePool.ObjectMeta.Namespace
	if exists, err := utils.HwMgrPluginNamespaceExists(ctx, t.client, pluginNameSpace); err != nil {
		return fmt.Errorf("failed check if hardware manager plugin namespace exists %s, err: %w", pluginNameSpace, err)
	} else if !exists {
		return fmt.Errorf("specified hardware manager plugin namespace does not exist: %s", pluginNameSpace)
	}

	// Create/update the clusterInstance namespace, adding ProvisioningRequest labels to the namespace
	err := t.createClusterInstanceNamespace(ctx, nodePool.GetName())
	if err != nil {
		return err
	}

	// Create the node pool resource
	createErr := utils.CreateK8sCR(ctx, t.client, nodePool, t.object, "")
	if createErr != nil {
		t.logger.ErrorContext(
			ctx,
			fmt.Sprintf(
				"Failed to create the NodePool %s in the namespace %s",
				nodePool.GetName(),
				nodePool.GetNamespace(),
			),
			slog.String("error", createErr.Error()),
		)
		return fmt.Errorf("failed to create/update the NodePool: %s", createErr.Error())
	}

	// Set NodePoolRef
	if t.object.Status.Extensions.NodePoolRef == nil {
		t.object.Status.Extensions.NodePoolRef = &provisioningv1alpha1.NodePoolRef{}
	}
	t.object.Status.Extensions.NodePoolRef.Name = nodePool.GetName()
	t.object.Status.Extensions.NodePoolRef.Namespace = nodePool.GetNamespace()
	// Set hardware provisioning start time after the NodePool is created
	currentTime := metav1.Now()
	t.object.Status.Extensions.NodePoolRef.HardwareProvisioningCheckStart = &currentTime

	err = utils.UpdateK8sCRStatus(ctx, t.client, t.object)
	if err != nil {
		return fmt.Errorf("failed to update status for ProvisioningRequest %s: %w", t.object.Name, err)
	}

	t.logger.InfoContext(
		ctx,
		fmt.Sprintf(
			"Created NodePool (%s) in the namespace %s, if not already exist",
			nodePool.GetName(),
			nodePool.GetNamespace(),
		),
	)

	return nil
}

// waitForHardwareData waits for the NodePool to be provisioned and update BMC details
// and bootMacAddress in ClusterInstance.
func (t *provisioningRequestReconcilerTask) waitForHardwareData(ctx context.Context,
	clusterInstance *unstructured.Unstructured, nodePool *hwv1alpha1.NodePool) (bool, *bool, bool, error) {

	var configured *bool
	provisioned, timedOutOrFailed, err := t.checkNodePoolProvisionStatus(ctx, clusterInstance, nodePool)
	if err != nil {
		return provisioned, nil, timedOutOrFailed, err
	}
	if provisioned {
		configured, timedOutOrFailed, err = t.checkNodePoolConfigStatus(ctx, nodePool)
	}
	return provisioned, configured, timedOutOrFailed, err
}

// updateClusterInstance updates the given ClusterInstance object based on the provisioned nodePool.
func (t *provisioningRequestReconcilerTask) updateClusterInstance(ctx context.Context,
	clusterInstance *unstructured.Unstructured, nodePool *hwv1alpha1.NodePool) error {

	hwNodes, err := utils.CollectNodeDetails(ctx, t.client, nodePool)
	if err != nil {
		return fmt.Errorf("failed to collect hardware node %s details for node pool: %w", nodePool.GetName(), err)
	}
	if nodePool.Spec.HwMgrId != utils.Metal3PluginName {
		if err := utils.CopyBMCSecrets(ctx, t.client, hwNodes, nodePool); err != nil {
			return fmt.Errorf("failed to copy BMC secret: %w", err)
		}
	} else {
		// The pull secret must be in the same namespace as the BMH.
		pullSecretName, err := utils.GetPullSecretName(clusterInstance)
		if err != nil {
			return fmt.Errorf("failed to get pull secret name from cluster instance: %w", err)
		}
		if err := utils.CopyPullSecret(ctx, t.client, t.object, t.ctDetails.namespace, pullSecretName, hwNodes); err != nil {
			return fmt.Errorf("failed to copy pull secret: %w", err)
		}
	}

	configErr := t.applyNodeConfiguration(ctx, hwNodes, nodePool, clusterInstance)
	if configErr != nil {
		msg := "Failed to apply node configuration to the rendered ClusterInstance: " + configErr.Error()
		utils.SetStatusCondition(&t.object.Status.Conditions,
			provisioningv1alpha1.PRconditionTypes.HardwareNodeConfigApplied,
			provisioningv1alpha1.CRconditionReasons.NotApplied,
			metav1.ConditionFalse,
			msg)
		utils.SetProvisioningStateFailed(t.object, msg)
	} else {
		utils.SetStatusCondition(&t.object.Status.Conditions,
			provisioningv1alpha1.PRconditionTypes.HardwareNodeConfigApplied,
			provisioningv1alpha1.CRconditionReasons.Completed,
			metav1.ConditionTrue,
			"Node configuration has been applied to the rendered ClusterInstance")
	}

	if updateErr := utils.UpdateK8sCRStatus(ctx, t.client, t.object); updateErr != nil {
		return fmt.Errorf("failed to update status for ProvisioningRequest %s: %w", t.object.Name, updateErr)
	}

	if configErr != nil {
		return fmt.Errorf("failed to apply node configuration for NodePool %s: %w", nodePool.GetName(), configErr)
	}
	return nil
}

// checkNodePoolStatus checks the NodePool status of a given condition type
// and updates the provisioning request status accordingly.
func (t *provisioningRequestReconcilerTask) checkNodePoolStatus(ctx context.Context,
	nodePool *hwv1alpha1.NodePool, condition hwv1alpha1.ConditionType) (bool, bool, error) {

	// Get the generated NodePool and its status.
	if err := utils.RetryOnConflictOrRetriableOrNotFound(retry.DefaultRetry, func() error {
		exists, err := utils.DoesK8SResourceExist(ctx, t.client, nodePool.GetName(),
			nodePool.GetNamespace(), nodePool)
		if err != nil {
			return fmt.Errorf("failed to get node pool; %w", err)
		}
		if !exists {
			return fmt.Errorf("node pool does not exist")
		}
		return nil
	}); err != nil {
		// nolint: wrapcheck
		return false, false, err
	}

	// Update the provisioning request Status with status from the NodePool object.
	status, timedOutOrFailed, err := t.updateHardwareStatus(ctx, nodePool, condition)
	if err != nil && !utils.IsConditionDoesNotExistsErr(err) {
		t.logger.ErrorContext(
			ctx,
			"Failed to update the NodePool status for ProvisioningRequest",
			slog.String("name", t.object.Name),
			slog.String("error", err.Error()),
		)
	}

	return status, timedOutOrFailed, err
}

// checkNodePoolProvisionStatus checks the provisioned status of the node pool.
func (t *provisioningRequestReconcilerTask) checkNodePoolProvisionStatus(ctx context.Context,
	clusterInstance *unstructured.Unstructured, nodePool *hwv1alpha1.NodePool) (bool, bool, error) {

	provisioned, timedOutOrFailed, err := t.checkNodePoolStatus(ctx, nodePool, hwv1alpha1.Provisioned)
	if provisioned && err == nil {
		t.logger.InfoContext(
			ctx,
			fmt.Sprintf(
				"NodePool (%s) in the namespace %s is provisioned",
				nodePool.GetName(),
				nodePool.GetNamespace(),
			),
		)
		if err = t.updateClusterInstance(ctx, clusterInstance, nodePool); err != nil {
			return provisioned, timedOutOrFailed, fmt.Errorf("failed to update the rendered cluster instance: %w", err)
		}
	}

	return provisioned, timedOutOrFailed, err
}

// checkNodePoolConfigStatus checks the configured status of the node pool.
func (t *provisioningRequestReconcilerTask) checkNodePoolConfigStatus(ctx context.Context, nodePool *hwv1alpha1.NodePool) (*bool, bool, error) {

	status, timedOutOrFailed, err := t.checkNodePoolStatus(ctx, nodePool, hwv1alpha1.Configured)
	if err != nil {
		if utils.IsConditionDoesNotExistsErr(err) {
			// Condition does not exist, return nil (acceptable case)
			return nil, timedOutOrFailed, nil
		}
		return nil, timedOutOrFailed, fmt.Errorf("failed to check NodePool configured status: %w", err)
	}
	return &status, timedOutOrFailed, nil
}

// applyNodeConfiguration updates the clusterInstance with BMC details, interface MACAddress and bootMACAddress
func (t *provisioningRequestReconcilerTask) applyNodeConfiguration(ctx context.Context, hwNodes map[string][]utils.NodeInfo,
	nodePool *hwv1alpha1.NodePool, clusterInstance *unstructured.Unstructured) error {

	// Create a map to track unmatched nodes
	unmatchedNodes := make(map[int]string)

	roleToNodeGroupName := utils.GetRoleToGroupNameMap(nodePool)

	// Extract the nodes slice
	nodes, found, err := unstructured.NestedSlice(clusterInstance.Object, "spec", "nodes")
	if err != nil {
		return fmt.Errorf("failed to extract nodes from cluster instance: %w", err)
	}
	if !found {
		return fmt.Errorf("spec.nodes not found in cluster instance")
	}

	for i, n := range nodes {
		nodeMap, ok := n.(map[string]interface{})
		if !ok {
			return fmt.Errorf("node at index %d is not a valid map", i)
		}

		role, _, _ := unstructured.NestedString(nodeMap, "role")
		hostName, _, _ := unstructured.NestedString(nodeMap, "hostName")
		groupName := roleToNodeGroupName[role]

		nodeInfos, exists := hwNodes[groupName]
		if !exists || len(nodeInfos) == 0 {
			unmatchedNodes[i] = hostName
			continue
		}

		// Make a copy of the nodeMap before mutating
		updatedNode := maps.Clone(nodeMap)

		// Set BMC info
		updatedNode["bmcAddress"] = nodeInfos[0].BmcAddress
		updatedNode["bmcCredentialsName"] = map[string]interface{}{
			"name": nodeInfos[0].BmcCredentials,
		}

		if nodeInfos[0].HwMgrNodeId != "" && nodeInfos[0].HwMgrNodeNs != "" {
			hostRef, ok := updatedNode["hostRef"].(map[string]interface{})
			if !ok {
				hostRef = make(map[string]interface{})
			}
			hostRef["name"] = nodeInfos[0].HwMgrNodeId
			hostRef["namespace"] = nodeInfos[0].HwMgrNodeNs
			updatedNode["hostRef"] = hostRef
		}
		// Boot MAC
		bootMAC, err := utils.GetBootMacAddress(nodeInfos[0].Interfaces, nodePool)
		if err != nil {
			return fmt.Errorf("failed to get boot MAC for node '%s': %w", hostName, err)
		}
		updatedNode["bootMACAddress"] = bootMAC

		// Assign MACs to interfaces
		if err := utils.AssignMacAddress(t.clusterInput.clusterInstanceData, nodeInfos[0].Interfaces, updatedNode); err != nil {
			return fmt.Errorf("failed to assign MACs for node '%s': %w", hostName, err)
		}

		// Update node status
		if err := utils.UpdateNodeStatusWithHostname(ctx, t.client, nodeInfos[0].NodeName, hostName, nodePool.Namespace); err != nil {
			return fmt.Errorf("failed to update status for node '%s': %w", hostName, err)
		}

		// Update the node only after all mutations succeed
		nodes[i] = updatedNode

		// Consume the nodeInfo
		hwNodes[groupName] = nodeInfos[1:]
	}

	// Final write back to clusterInstance
	if err := unstructured.SetNestedSlice(clusterInstance.Object, nodes, "spec", "nodes"); err != nil {
		return fmt.Errorf("failed to update nodes in cluster instance: %w", err)
	}
	// Check if there are unmatched nodes
	if len(unmatchedNodes) > 0 {
		var unmatchedDetails []string
		for idx, name := range unmatchedNodes {
			unmatchedDetails = append(unmatchedDetails, fmt.Sprintf("Index: %d, Host Name: %s", idx, name))
		}
		return fmt.Errorf("failed to find matches for the following nodes: %s", strings.Join(unmatchedDetails, "; "))
	}

	return nil
}

// updateHardwareStatus updates the hardware status for the ProvisioningRequest
func (t *provisioningRequestReconcilerTask) updateHardwareStatus(
	ctx context.Context, nodePool *hwv1alpha1.NodePool, condition hwv1alpha1.ConditionType) (bool, bool, error) {
	if t.object.Status.Extensions.NodePoolRef == nil {
		return false, false, fmt.Errorf("status.nodePoolRef is empty")
	}

	var (
		status  metav1.ConditionStatus
		reason  string
		message string
		err     error
	)
	timedOutOrFailed := false // Default to false unless explicitly needed

	// Retrieve the given hardware condition(Provisioned or Configured) from the nodePool status.
	hwCondition := meta.FindStatusCondition(nodePool.Status.Conditions, string(condition))
	if hwCondition == nil {
		// Condition does not exist
		status = metav1.ConditionUnknown
		reason = string(provisioningv1alpha1.CRconditionReasons.Unknown)
		message = fmt.Sprintf("Waiting for NodePool (%s) to be processed", nodePool.GetName())

		if condition == hwv1alpha1.Configured {
			// If there was no hardware configuration update initiated, return a custom error to
			// indicate that the configured condition does not exist.
			if t.object.Status.Extensions.NodePoolRef.HardwareConfiguringCheckStart.IsZero() {
				return false, false, &utils.ConditionDoesNotExistsErr{ConditionName: string(condition)}
			}
		}
		utils.SetProvisioningStateInProgress(t.object, message)
	} else {
		// A hardware condition was found; use its details.
		status = hwCondition.Status
		reason = hwCondition.Reason
		message = hwCondition.Message

		// If the condition is Configured and it's completed, reset the configuring check start time.
		if hwCondition.Type == string(hwv1alpha1.Configured) && status == metav1.ConditionTrue {
			t.object.Status.Extensions.NodePoolRef.HardwareConfiguringCheckStart = nil
		} else if hwCondition.Type == string(hwv1alpha1.Configured) && t.object.Status.Extensions.NodePoolRef.HardwareConfiguringCheckStart == nil {
			// HardwareConfiguringCheckStart is nil, so reset it to current time
			currentTime := metav1.Now()
			t.object.Status.Extensions.NodePoolRef.HardwareConfiguringCheckStart = &currentTime
		}

		// Ensure a consistent message for the provisioning request, regardless of which plugin is used.
		if status == metav1.ConditionFalse {
			message = fmt.Sprintf("Hardware %s is in progress", utils.GetStatusMessage(condition))
			utils.SetProvisioningStateInProgress(t.object, message)

			if reason == string(hwv1alpha1.Failed) {
				timedOutOrFailed = true
				message = fmt.Sprintf("Hardware %s failed", utils.GetStatusMessage(condition))
				utils.SetProvisioningStateFailed(t.object, message)
			}
		}
	}

	// Unknown or in progress hardware status, check if it timed out
	if status != metav1.ConditionTrue && reason != string(hwv1alpha1.Failed) {
		// Handle timeout logic
		timedOutOrFailed, reason, message = utils.HandleHardwareTimeout(
			condition,
			t.object.Status.Extensions.NodePoolRef.HardwareProvisioningCheckStart,
			t.object.Status.Extensions.NodePoolRef.HardwareConfiguringCheckStart,
			t.timeouts.hardwareProvisioning,
			reason,
			message,
		)
		if timedOutOrFailed {
			utils.SetProvisioningStateFailed(t.object, message)
		}
	}

	conditionType := provisioningv1alpha1.PRconditionTypes.HardwareProvisioned
	if condition == hwv1alpha1.Configured {
		conditionType = provisioningv1alpha1.PRconditionTypes.HardwareConfigured
	}

	// Set the status condition for hardware status.
	utils.SetStatusCondition(&t.object.Status.Conditions,
		conditionType,
		provisioningv1alpha1.ConditionReason(reason),
		status,
		message)
	t.logger.InfoContext(ctx, fmt.Sprintf("NodePool (%s) %s status: %s",
		nodePool.GetName(), utils.GetStatusMessage(condition), message))

	// Update the CR status for the ProvisioningRequest.
	if err = utils.UpdateK8sCRStatus(ctx, t.client, t.object); err != nil {
		err = fmt.Errorf("failed to update Hardware %s status: %w", utils.GetStatusMessage(condition), err)
	}
	return status == metav1.ConditionTrue, timedOutOrFailed, err
}

// checkExistingNodePool checks for an existing NodePool and verifies changes if necessary
func (t *provisioningRequestReconcilerTask) checkExistingNodePool(ctx context.Context, clusterInstance *unstructured.Unstructured,
	hwTemplate *hwv1alpha1.HardwareTemplate, nodePool *hwv1alpha1.NodePool) error {

	ns := utils.GetHwMgrPluginNS()
	exist, err := utils.DoesK8SResourceExist(ctx, t.client, clusterInstance.GetName(), ns, nodePool)
	if err != nil {
		return fmt.Errorf("failed to get NodePool %s in namespace %s: %w", clusterInstance.GetName(), ns, err)
	}

	if exist {
		_, err := utils.CompareHardwareTemplateWithNodePool(hwTemplate, nodePool)
		if err != nil {
			return utils.NewInputError("%w", err)
		}
	}

	return nil
}

// buildNodePoolSpec builds the NodePool spec based on the templates and cluster instance
func (t *provisioningRequestReconcilerTask) buildNodePoolSpec(clusterInstance *unstructured.Unstructured,
	hwTemplate *hwv1alpha1.HardwareTemplate, nodePool *hwv1alpha1.NodePool) error {

	roleCounts := make(map[string]int)
	nodes, found, err := unstructured.NestedSlice(clusterInstance.Object, "spec", "nodes")
	if err != nil {
		return fmt.Errorf("failed to extract nodes from cluster instance: %w", err)
	}
	if !found {
		return fmt.Errorf("spec.nodes not found in cluster instance")
	}

	for i, n := range nodes {
		nodeMap, ok := n.(map[string]interface{})
		if !ok {
			return fmt.Errorf("node at index %d is not a valid map", i)
		}

		role, _, _ := unstructured.NestedString(nodeMap, "role")
		roleCounts[role]++
	}

	nodeGroups := []hwv1alpha1.NodeGroup{}
	for _, group := range hwTemplate.Spec.NodePoolData {
		nodeGroup := utils.NewNodeGroup(group, roleCounts)
		nodeGroups = append(nodeGroups, nodeGroup)
	}

	siteID, err := provisioningv1alpha1.ExtractMatchingInput(
		t.object.Spec.TemplateParameters.Raw, utils.TemplateParamOCloudSiteId)
	if err != nil {
		return fmt.Errorf("failed to get %s from templateParameters: %w", utils.TemplateParamOCloudSiteId, err)
	}

	nodePool.Spec.CloudID = clusterInstance.GetName()
	nodePool.Spec.Site = siteID.(string)
	nodePool.Spec.HwMgrId = hwTemplate.Spec.HwMgrId
	nodePool.Spec.Extensions = hwTemplate.Spec.Extensions
	nodePool.Spec.NodeGroup = nodeGroups
	nodePool.ObjectMeta.Name = clusterInstance.GetName()
	nodePool.ObjectMeta.Namespace = utils.GetHwMgrPluginNS()

	// Add boot interface label annotation to the generated nodePool
	utils.SetNodePoolAnnotations(nodePool, hwv1alpha1.BootInterfaceLabelAnnotation, hwTemplate.Spec.BootInterfaceLabel)
	// Add ProvisioningRequest labels to the generated nodePool
	utils.SetNodePoolLabels(nodePool, provisioningv1alpha1.ProvisioningRequestNameLabel, t.object.Name)

	return nil
}

func (t *provisioningRequestReconcilerTask) handleRenderHardwareTemplate(ctx context.Context,
	clusterInstance *unstructured.Unstructured) (*hwv1alpha1.NodePool, error) {

	nodePool := &hwv1alpha1.NodePool{}

	clusterTemplate, err := t.object.GetClusterTemplateRef(ctx, t.client)
	if err != nil {
		return nil, fmt.Errorf("failed to get the ClusterTemplate for ProvisioningRequest %s: %w ", t.object.Name, err)
	}

	hwTemplateName := clusterTemplate.Spec.Templates.HwTemplate
	hwTemplate, err := utils.GetHardwareTemplate(ctx, t.client, hwTemplateName)
	if err != nil {
		return nil, fmt.Errorf("failed to get the HardwareTemplate %s resource: %w ", hwTemplateName, err)
	}

	if err := t.checkExistingNodePool(ctx, clusterInstance, hwTemplate, nodePool); err != nil {
		if utils.IsInputError(err) {
			updateErr := utils.UpdateHardwareTemplateStatusCondition(ctx, t.client, hwTemplate, provisioningv1alpha1.ConditionType(hwv1alpha1.Validation),
				provisioningv1alpha1.ConditionReason(hwv1alpha1.Failed), metav1.ConditionFalse, err.Error())
			if updateErr != nil {
				// nolint: wrapcheck
				return nil, updateErr
			}
		}
		return nil, err
	}

	hwmgr := &pluginv1alpha1.HardwareManager{}
	if err := t.client.Get(ctx, types.NamespacedName{Namespace: utils.GetHwMgrPluginNS(), Name: hwTemplate.Spec.HwMgrId}, hwmgr); err != nil {
		updateErr := utils.UpdateHardwareTemplateStatusCondition(ctx, t.client, hwTemplate, provisioningv1alpha1.ConditionType(hwv1alpha1.Validation),
			provisioningv1alpha1.ConditionReason(hwv1alpha1.Failed), metav1.ConditionFalse,
			"Unable to find specified HardwareManager: "+hwTemplate.Spec.HwMgrId)
		if updateErr != nil {
			return nil, fmt.Errorf("failed to update hwtemplate %s status: %w", hwTemplateName, updateErr)
		}
		return nil, fmt.Errorf("could not find specified HardwareManager: %s/%s, err=%w", utils.GetHwMgrPluginNS(), hwTemplate.Spec.HwMgrId, err)
	}

	// The HardwareTemplate is validated by the CRD schema and no additional validation is needed
	updateErr := utils.UpdateHardwareTemplateStatusCondition(ctx, t.client, hwTemplate, provisioningv1alpha1.ConditionType(hwv1alpha1.Validation),
		provisioningv1alpha1.ConditionReason(hwv1alpha1.Completed), metav1.ConditionTrue, "Validated")
	if updateErr != nil {
		// nolint: wrapcheck
		return nil, updateErr
	}

	if err := t.buildNodePoolSpec(clusterInstance, hwTemplate, nodePool); err != nil {
		return nil, err
	}

	return nodePool, nil
}
