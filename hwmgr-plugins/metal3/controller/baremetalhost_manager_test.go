/*
SPDX-FileCopyrightText: Red Hat

SPDX-License-Identifier: Apache-2.0
*/

/*
Generated-By: Cursor/claude-4-sonnet
*/

/*
Package controller provides unit tests for BareMetalHost (BMH) management functionality
in the Metal3 hardware plugin controller.

This test file contains comprehensive test coverage for the following areas:

BMH Status and Allocation Management:
- Testing BMH allocation status constants and helper functions
- Validating BMH allocation state checking and filtering
- Testing BMH allocation marking and deallocation workflows

BMH Grouping and Organization:
- Testing BMH grouping by resource pools
- Validating BMH filtering by availability status
- Testing BMH list fetching with various filter criteria

BMH Network and Interface Management:
- Testing interface building from BMH hardware details
- Validating network data clearing and configuration
- Testing boot interface identification and labeling

BMH Metadata and Annotation Management:
- Testing label and annotation operations (add/remove)
- Validating BMH metadata updates with retry logic
- Testing infrastructure environment label management

BMH Lifecycle Operations:
- Testing BMH host management permission settings
- Validating BMH finalization and cleanup procedures
- Testing BMH reboot annotation management

Node and Hardware Integration:
- Testing AllocatedNode to BMH relationships
- Validating node configuration progress tracking
- Testing node grouping and counting operations

Supporting Infrastructure:
- Testing PreprovisioningImage label management
- Validating BMC information handling
- Testing error handling and edge cases

The tests use Ginkgo/Gomega testing framework with fake Kubernetes clients
to simulate controller operations without requiring actual cluster resources.
*/

package controller

import (
	"context"
	"log/slog"

	metal3v1alpha1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pluginsv1alpha1 "github.com/openshift-kni/oran-o2ims/api/hardwaremanagement/plugins/v1alpha1"
	hwmgmtv1alpha1 "github.com/openshift-kni/oran-o2ims/api/hardwaremanagement/v1alpha1"
)

const (
	nonexistentBMHID = "nonexistent-bmh"
)

var _ = Describe("BareMetalHost Manager", func() {
	var (
		ctx    context.Context
		logger *slog.Logger
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		logger = slog.Default()
		scheme = runtime.NewScheme()
		Expect(metal3v1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(pluginsv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(hwmgmtv1alpha1.AddToScheme(scheme)).To(Succeed())
	})

	// Helper functions
	createBMH := func(name, namespace string, labels map[string]string, annotations map[string]string, state metal3v1alpha1.ProvisioningState) *metal3v1alpha1.BareMetalHost {
		bmh := &metal3v1alpha1.BareMetalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   namespace,
				Labels:      labels,
				Annotations: annotations,
			},
			Status: metal3v1alpha1.BareMetalHostStatus{
				Provisioning: metal3v1alpha1.ProvisionStatus{
					State: state,
				},
				HardwareDetails: &metal3v1alpha1.HardwareDetails{
					NIC: []metal3v1alpha1.NIC{
						{
							Name: "eth0",
							MAC:  "00:11:22:33:44:55",
						},
						{
							Name: "eth1",
							MAC:  "00:11:22:33:44:56",
						},
					},
				},
			},
		}
		return bmh
	}

	createNodeAllocationRequest := func(name, namespace string) *pluginsv1alpha1.NodeAllocationRequest {
		return &pluginsv1alpha1.NodeAllocationRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: pluginsv1alpha1.NodeAllocationRequestSpec{
				BootInterfaceLabel: "boot",
			},
		}
	}

	createAllocatedNode := func(name, namespace, hwMgrNodeId, hwMgrNodeNs string) *pluginsv1alpha1.AllocatedNode {
		return &pluginsv1alpha1.AllocatedNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: pluginsv1alpha1.AllocatedNodeSpec{
				HwMgrNodeId: hwMgrNodeId,
				HwMgrNodeNs: hwMgrNodeNs,
			},
			Status: pluginsv1alpha1.AllocatedNodeStatus{
				Conditions: []metav1.Condition{},
			},
		}
	}

	Describe("BMHAllocationStatus constants", func() {
		It("should have correct values", func() {
			Expect(string(AllBMHs)).To(Equal("all"))
			Expect(string(UnallocatedBMHs)).To(Equal("unallocated"))
			Expect(string(AllocatedBMHs)).To(Equal("allocated"))
		})
	})

	Describe("isBMHAllocated", func() {
		It("should return true when BMH has allocated label set to true", func() {
			bmh := createBMH("test-bmh", "test-ns", map[string]string{
				BmhAllocatedLabel: ValueTrue,
			}, nil, metal3v1alpha1.StateAvailable)

			result := isBMHAllocated(bmh)
			Expect(result).To(BeTrue())
		})

		It("should return false when BMH has allocated label set to false", func() {
			bmh := createBMH("test-bmh", "test-ns", map[string]string{
				BmhAllocatedLabel: "false",
			}, nil, metal3v1alpha1.StateAvailable)

			result := isBMHAllocated(bmh)
			Expect(result).To(BeFalse())
		})

		It("should return false when BMH has no allocated label", func() {
			bmh := createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)

			result := isBMHAllocated(bmh)
			Expect(result).To(BeFalse())
		})
	})

	Describe("filterAvailableBMHs", func() {
		It("should filter out non-available BMHs", func() {
			bmhList := metal3v1alpha1.BareMetalHostList{
				Items: []metal3v1alpha1.BareMetalHost{
					*createBMH("bmh1", "test-ns", nil, nil, metal3v1alpha1.StateAvailable),
					*createBMH("bmh2", "test-ns", nil, nil, metal3v1alpha1.StateProvisioning),
					*createBMH("bmh3", "test-ns", nil, nil, metal3v1alpha1.StateAvailable),
					*createBMH("bmh4", "test-ns", nil, nil, metal3v1alpha1.StateDeprovisioning),
				},
			}

			filtered := filterAvailableBMHs(bmhList)
			Expect(len(filtered.Items)).To(Equal(2))
			Expect(filtered.Items[0].Name).To(Equal("bmh1"))
			Expect(filtered.Items[1].Name).To(Equal("bmh3"))
		})

		It("should return empty list when no BMHs are available", func() {
			bmhList := metal3v1alpha1.BareMetalHostList{
				Items: []metal3v1alpha1.BareMetalHost{
					*createBMH("bmh1", "test-ns", nil, nil, metal3v1alpha1.StateProvisioning),
					*createBMH("bmh2", "test-ns", nil, nil, metal3v1alpha1.StateDeprovisioning),
				},
			}

			filtered := filterAvailableBMHs(bmhList)
			Expect(len(filtered.Items)).To(Equal(0))
		})
	})

	Describe("GroupBMHsByResourcePool", func() {
		It("should group BMHs by resource pool ID", func() {
			bmhList := metal3v1alpha1.BareMetalHostList{
				Items: []metal3v1alpha1.BareMetalHost{
					*createBMH("bmh1", "test-ns", map[string]string{
						LabelResourcePoolID: "pool1",
					}, nil, metal3v1alpha1.StateAvailable),
					*createBMH("bmh2", "test-ns", map[string]string{
						LabelResourcePoolID: "pool2",
					}, nil, metal3v1alpha1.StateAvailable),
					*createBMH("bmh3", "test-ns", map[string]string{
						LabelResourcePoolID: "pool1",
					}, nil, metal3v1alpha1.StateAvailable),
				},
			}

			grouped := GroupBMHsByResourcePool(bmhList)
			Expect(len(grouped)).To(Equal(2))
			Expect(len(grouped["pool1"])).To(Equal(2))
			Expect(len(grouped["pool2"])).To(Equal(1))
			Expect(grouped["pool1"][0].Name).To(Equal("bmh1"))
			Expect(grouped["pool1"][1].Name).To(Equal("bmh3"))
			Expect(grouped["pool2"][0].Name).To(Equal("bmh2"))
		})

		It("should handle BMHs without resource pool label", func() {
			bmhList := metal3v1alpha1.BareMetalHostList{
				Items: []metal3v1alpha1.BareMetalHost{
					*createBMH("bmh1", "test-ns", map[string]string{
						LabelResourcePoolID: "pool1",
					}, nil, metal3v1alpha1.StateAvailable),
					*createBMH("bmh2", "test-ns", nil, nil, metal3v1alpha1.StateAvailable),
				},
			}

			grouped := GroupBMHsByResourcePool(bmhList)
			Expect(len(grouped)).To(Equal(1))
			Expect(len(grouped["pool1"])).To(Equal(1))
		})
	})

	Describe("buildInterfacesFromBMH", func() {
		It("should build interfaces correctly with boot interface", func() {
			nodeRequest := createNodeAllocationRequest("test-request", "test-ns")
			bmh := createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			bmh.Spec.BootMACAddress = "00:11:22:33:44:55"

			interfaces, err := buildInterfacesFromBMH(nodeRequest, bmh)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(interfaces)).To(Equal(2))

			// Find boot interface
			var bootInterface *pluginsv1alpha1.Interface
			for _, iface := range interfaces {
				if iface.MACAddress == "00:11:22:33:44:55" {
					bootInterface = iface
					break
				}
			}
			Expect(bootInterface).NotTo(BeNil())
			Expect(bootInterface.Label).To(Equal("boot"))
			Expect(bootInterface.Name).To(Equal("eth0"))
		})

		It("should handle interface labels with MAC addresses", func() {
			nodeRequest := createNodeAllocationRequest("test-request", "test-ns")
			bmh := createBMH("test-bmh", "test-ns", map[string]string{
				"interfacelabel.clcm.openshift.io/mgmt": "00-11-22-33-44-56",
			}, nil, metal3v1alpha1.StateAvailable)

			interfaces, err := buildInterfacesFromBMH(nodeRequest, bmh)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(interfaces)).To(Equal(2))

			// Find labeled interface
			var labeledInterface *pluginsv1alpha1.Interface
			for _, iface := range interfaces {
				if iface.MACAddress == "00:11:22:33:44:56" {
					labeledInterface = iface
					break
				}
			}
			Expect(labeledInterface).NotTo(BeNil())
			Expect(labeledInterface.Label).To(Equal("mgmt"))
		})

		It("should return error when hardware details are nil", func() {
			nodeRequest := createNodeAllocationRequest("test-request", "test-ns")
			bmh := createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			bmh.Status.HardwareDetails = nil

			_, err := buildInterfacesFromBMH(nodeRequest, bmh)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bareMetalHost.status.hardwareDetails should not be nil"))
		})
	})

	Describe("checkBMHStatus", func() {
		It("should return true when BMH is in desired state", func() {
			bmh := createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)

			result := checkBMHStatus(ctx, logger, bmh, metal3v1alpha1.StateAvailable)
			Expect(result).To(BeTrue())
		})

		It("should return false when BMH is not in desired state", func() {
			bmh := createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateProvisioning)

			result := checkBMHStatus(ctx, logger, bmh, metal3v1alpha1.StateAvailable)
			Expect(result).To(BeFalse())
		})
	})

	Describe("updateBMHMetaWithRetry", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()
		})

		It("should add label successfully", func() {
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := updateBMHMetaWithRetry(ctx, fakeClient, logger, name, MetaTypeLabel, "test-key", "test-value", OpAdd)
			Expect(err).NotTo(HaveOccurred())

			// Verify label was added
			var updatedBMH metal3v1alpha1.BareMetalHost
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedBMH.Labels["test-key"]).To(Equal("test-value"))
		})

		It("should add annotation successfully", func() {
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := updateBMHMetaWithRetry(ctx, fakeClient, logger, name, MetaTypeAnnotation, "test-key", "test-value", OpAdd)
			Expect(err).NotTo(HaveOccurred())

			// Verify annotation was added
			var updatedBMH metal3v1alpha1.BareMetalHost
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedBMH.Annotations["test-key"]).To(Equal("test-value"))
		})

		It("should remove label successfully", func() {
			// First add a label
			bmh.Labels = map[string]string{"test-key": "test-value"}
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()

			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := updateBMHMetaWithRetry(ctx, fakeClient, logger, name, MetaTypeLabel, "test-key", "", OpRemove)
			Expect(err).NotTo(HaveOccurred())

			// Verify label was removed
			var updatedBMH metal3v1alpha1.BareMetalHost
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			_, exists := updatedBMH.Labels["test-key"]
			Expect(exists).To(BeFalse())
		})

		It("should handle unsupported meta type", func() {
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := updateBMHMetaWithRetry(ctx, fakeClient, logger, name, "invalid", "test-key", "test-value", OpAdd)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported meta type"))
		})

		It("should handle unsupported operation", func() {
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := updateBMHMetaWithRetry(ctx, fakeClient, logger, name, MetaTypeLabel, "test-key", "test-value", "invalid")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported operation"))
		})

		It("should skip remove operation when map is nil", func() {
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := updateBMHMetaWithRetry(ctx, fakeClient, logger, name, MetaTypeLabel, "test-key", "", OpRemove)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should skip remove operation when key doesn't exist", func() {
			bmh.Labels = map[string]string{"other-key": "other-value"}
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()

			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := updateBMHMetaWithRetry(ctx, fakeClient, logger, name, MetaTypeLabel, "test-key", "", OpRemove)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("clearBMHNetworkData", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			bmh.Spec.PreprovisioningNetworkDataName = "test-network-data"
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()
		})

		It("should clear network data successfully", func() {
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := clearBMHNetworkData(ctx, fakeClient, name)
			Expect(err).NotTo(HaveOccurred())

			// Verify network data was cleared
			var updatedBMH metal3v1alpha1.BareMetalHost
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedBMH.Spec.PreprovisioningNetworkDataName).To(Equal(""))
		})

		It("should succeed when network data is already empty", func() {
			bmh.Spec.PreprovisioningNetworkDataName = ""
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()

			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := clearBMHNetworkData(ctx, fakeClient, name)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("markBMHAllocated", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()
		})

		It("should mark BMH as allocated", func() {
			err := markBMHAllocated(ctx, fakeClient, logger, bmh)
			Expect(err).NotTo(HaveOccurred())

			// Verify BMH was marked as allocated
			var updatedBMH metal3v1alpha1.BareMetalHost
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedBMH.Labels[BmhAllocatedLabel]).To(Equal(ValueTrue))
		})

		It("should skip update when BMH is already allocated", func() {
			bmh.Labels = map[string]string{BmhAllocatedLabel: ValueTrue}
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()

			err := markBMHAllocated(ctx, fakeClient, logger, bmh)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("allowHostManagement", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()
		})

		It("should add host management annotation", func() {
			err := allowHostManagement(ctx, fakeClient, logger, bmh)
			Expect(err).NotTo(HaveOccurred())

			// Verify annotation was added
			var updatedBMH metal3v1alpha1.BareMetalHost
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			_, exists := updatedBMH.Annotations[BmhHostMgmtAnnotation]
			Expect(exists).To(BeTrue())
		})

		It("should skip when annotation already exists with empty value", func() {
			bmh.Annotations = map[string]string{BmhHostMgmtAnnotation: ""}
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()

			err := allowHostManagement(ctx, fakeClient, logger, bmh)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("getBMHForNode", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
			node       *pluginsv1alpha1.AllocatedNode
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			node = createAllocatedNode("test-node", "test-ns", "test-bmh", "test-ns")
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh, node).Build()
		})

		It("should return BMH for node successfully", func() {
			retrievedBMH, err := getBMHForNode(ctx, fakeClient, node)
			Expect(err).NotTo(HaveOccurred())
			Expect(retrievedBMH.Name).To(Equal("test-bmh"))
			Expect(retrievedBMH.Namespace).To(Equal("test-ns"))
		})

		It("should return error when BMH not found", func() {
			node.Spec.HwMgrNodeId = nonexistentBMHID
			_, err := getBMHForNode(ctx, fakeClient, node)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unable to find BMH"))
		})
	})

	Describe("fetchBMHList", func() {
		var (
			fakeClient client.Client
			bmh1, bmh2 *metal3v1alpha1.BareMetalHost
			nodeGroup  hwmgmtv1alpha1.NodeGroupData
		)

		BeforeEach(func() {
			bmh1 = createBMH("bmh1", "test-ns", map[string]string{
				LabelSiteID:         "site1",
				LabelResourcePoolID: "pool1",
				BmhAllocatedLabel:   ValueTrue,
			}, nil, metal3v1alpha1.StateAvailable)

			bmh2 = createBMH("bmh2", "test-ns", map[string]string{
				LabelSiteID:         "site1",
				LabelResourcePoolID: "pool1",
			}, nil, metal3v1alpha1.StateAvailable)

			nodeGroup = hwmgmtv1alpha1.NodeGroupData{
				ResourcePoolId:   "pool1",
				ResourceSelector: map[string]string{},
			}

			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh1, bmh2).Build()
		})

		It("should fetch all BMHs", func() {
			bmhList, err := fetchBMHList(ctx, fakeClient, logger, "site1", nodeGroup, AllBMHs, "test-ns")
			Expect(err).NotTo(HaveOccurred())
			Expect(len(bmhList.Items)).To(Equal(2))
		})

		It("should fetch only allocated BMHs", func() {
			bmhList, err := fetchBMHList(ctx, fakeClient, logger, "site1", nodeGroup, AllocatedBMHs, "test-ns")
			Expect(err).NotTo(HaveOccurred())
			Expect(len(bmhList.Items)).To(Equal(1))
			Expect(bmhList.Items[0].Name).To(Equal("bmh1"))
		})

		It("should fetch only unallocated BMHs", func() {
			bmhList, err := fetchBMHList(ctx, fakeClient, logger, "site1", nodeGroup, UnallocatedBMHs, "test-ns")
			Expect(err).NotTo(HaveOccurred())
			Expect(len(bmhList.Items)).To(Equal(1))
			Expect(bmhList.Items[0].Name).To(Equal("bmh2"))
		})

		It("should filter by site ID", func() {
			bmh3 := createBMH("bmh3", "test-ns", map[string]string{
				LabelSiteID:         "site2",
				LabelResourcePoolID: "pool1",
			}, nil, metal3v1alpha1.StateAvailable)
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh1, bmh2, bmh3).Build()

			bmhList, err := fetchBMHList(ctx, fakeClient, logger, "site1", nodeGroup, AllBMHs, "test-ns")
			Expect(err).NotTo(HaveOccurred())
			Expect(len(bmhList.Items)).To(Equal(2))
		})

		It("should filter by resource pool ID", func() {
			bmh3 := createBMH("bmh3", "test-ns", map[string]string{
				LabelSiteID:         "site1",
				LabelResourcePoolID: "pool2",
			}, nil, metal3v1alpha1.StateAvailable)
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh1, bmh2, bmh3).Build()

			bmhList, err := fetchBMHList(ctx, fakeClient, logger, "site1", nodeGroup, AllBMHs, "test-ns")
			Expect(err).NotTo(HaveOccurred())
			Expect(len(bmhList.Items)).To(Equal(2))
		})
	})

	Describe("finalizeBMHDeallocation", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", map[string]string{
				SiteConfigOwnedByLabel:     "test-cluster",
				BmhAllocatedLabel:          ValueTrue,
				"utils.AllocatedNodeLabel": "test-node",
			}, map[string]string{
				BiosUpdateNeededAnnotation:     ValueTrue,
				FirmwareUpdateNeededAnnotation: ValueTrue,
			}, metal3v1alpha1.StateProvisioned)

			// Set up CustomDeploy and Image
			bmh.Spec.CustomDeploy = &metal3v1alpha1.CustomDeploy{
				Method: "test-method",
			}
			bmh.Spec.Image = &metal3v1alpha1.Image{
				URL: "test-image-url",
			}
			bmh.Spec.Online = true
			bmh.Spec.PreprovisioningNetworkDataName = "old-network-data"

			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()
		})

		It("should deallocate BMH successfully", func() {
			err := finalizeBMHDeallocation(ctx, fakeClient, logger, bmh)
			Expect(err).NotTo(HaveOccurred())

			// Verify BMH was deallocated correctly
			var updatedBMH metal3v1alpha1.BareMetalHost
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())

			// Check that allocation labels were removed
			_, hasOwnedBy := updatedBMH.Labels[SiteConfigOwnedByLabel]
			Expect(hasOwnedBy).To(BeFalse())
			_, hasAllocated := updatedBMH.Labels[BmhAllocatedLabel]
			Expect(hasAllocated).To(BeFalse())

			// Check that configuration annotations were removed
			_, hasBiosAnnotation := updatedBMH.Annotations[BiosUpdateNeededAnnotation]
			Expect(hasBiosAnnotation).To(BeFalse())
			_, hasFirmwareAnnotation := updatedBMH.Annotations[FirmwareUpdateNeededAnnotation]
			Expect(hasFirmwareAnnotation).To(BeFalse())

			// Check that spec fields were updated
			Expect(updatedBMH.Spec.Online).To(BeFalse())
			Expect(updatedBMH.Spec.CustomDeploy).To(BeNil())
			Expect(updatedBMH.Spec.Image).To(BeNil())
			Expect(updatedBMH.Spec.PreprovisioningNetworkDataName).To(Equal(BmhNetworkDataPrefx + "-" + bmh.Name))
		})

		It("should set automated cleaning mode for provisioned BMH", func() {
			bmh.Status.Provisioning.State = metal3v1alpha1.StateProvisioned
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()

			err := finalizeBMHDeallocation(ctx, fakeClient, logger, bmh)
			Expect(err).NotTo(HaveOccurred())

			var updatedBMH metal3v1alpha1.BareMetalHost
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedBMH.Spec.AutomatedCleaningMode).To(Equal(metal3v1alpha1.CleaningModeMetadata))
		})
	})

	Describe("removeInfraEnvLabelFromPreprovisioningImage", func() {
		var (
			fakeClient client.Client
			image      *metal3v1alpha1.PreprovisioningImage
		)

		BeforeEach(func() {
			image = &metal3v1alpha1.PreprovisioningImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-ns",
					Labels: map[string]string{
						BmhInfraEnvLabel: "test-infraenv",
						"other-label":    "other-value",
					},
				},
			}
			// Add PreprovisioningImage to scheme
			Expect(metal3v1alpha1.AddToScheme(scheme)).To(Succeed())
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
		})

		It("should remove InfraEnv label from PreprovisioningImage", func() {
			name := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}
			err := removeInfraEnvLabelFromPreprovisioningImage(ctx, fakeClient, logger, name)
			Expect(err).NotTo(HaveOccurred())

			// Verify label was removed
			var updatedImage metal3v1alpha1.PreprovisioningImage
			err = fakeClient.Get(ctx, name, &updatedImage)
			Expect(err).NotTo(HaveOccurred())
			_, exists := updatedImage.Labels[BmhInfraEnvLabel]
			Expect(exists).To(BeFalse())
			// Other labels should remain
			Expect(updatedImage.Labels["other-label"]).To(Equal("other-value"))
		})
	})

	Describe("annotateNodeConfigInProgress", func() {
		var (
			fakeClient client.Client
			node       *pluginsv1alpha1.AllocatedNode
		)

		BeforeEach(func() {
			node = createAllocatedNode("test-node", "test-ns", "test-bmh", "test-ns")
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
		})

		It("should annotate node with config in progress", func() {
			err := annotateNodeConfigInProgress(ctx, fakeClient, logger, "test-ns", "test-node", UpdateReasonBIOSSettings)
			Expect(err).NotTo(HaveOccurred())

			// Verify annotation was added
			var updatedNode pluginsv1alpha1.AllocatedNode
			err = fakeClient.Get(ctx, types.NamespacedName{Name: "test-node", Namespace: "test-ns"}, &updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations[ConfigAnnotation]).To(Equal(UpdateReasonBIOSSettings))
		})

		It("should handle node with existing annotations", func() {
			node.Annotations = map[string]string{"existing": "value"}
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

			err := annotateNodeConfigInProgress(ctx, fakeClient, logger, "test-ns", "test-node", UpdateReasonFirmware)
			Expect(err).NotTo(HaveOccurred())

			// Verify both annotations exist
			var updatedNode pluginsv1alpha1.AllocatedNode
			err = fakeClient.Get(ctx, types.NamespacedName{Name: "test-node", Namespace: "test-ns"}, &updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations[ConfigAnnotation]).To(Equal(UpdateReasonFirmware))
			Expect(updatedNode.Annotations["existing"]).To(Equal("value"))
		})
	})

	Describe("countNodesInGroup", func() {
		var (
			fakeClient client.Reader
			node1      *pluginsv1alpha1.AllocatedNode
			node2      *pluginsv1alpha1.AllocatedNode
		)

		BeforeEach(func() {
			node1 = createAllocatedNode("node1", "test-ns", "bmh1", "test-ns")
			node1.Spec.GroupName = "group1"
			node2 = createAllocatedNode("node2", "test-ns", "bmh2", "test-ns")
			node2.Spec.GroupName = "group2"
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node1, node2).Build()
		})

		It("should count nodes in specific group", func() {
			nodeNames := []string{"node1", "node2"}
			count := countNodesInGroup(ctx, fakeClient, logger, "test-ns", nodeNames, "group1")
			Expect(count).To(Equal(1))
		})

		It("should return zero for non-existent group", func() {
			nodeNames := []string{"node1", "node2"}
			count := countNodesInGroup(ctx, fakeClient, logger, "test-ns", nodeNames, "nonexistent")
			Expect(count).To(Equal(0))
		})

		It("should handle non-existent nodes gracefully", func() {
			nodeNames := []string{"node1", "nonexistent-node"}
			count := countNodesInGroup(ctx, fakeClient, logger, "test-ns", nodeNames, "group1")
			Expect(count).To(Equal(1))
		})
	})

	Describe("addRebootAnnotation", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", nil, nil, metal3v1alpha1.StateAvailable)
			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh).Build()
		})

		It("should add reboot annotation to BMH", func() {
			err := addRebootAnnotation(ctx, fakeClient, logger, bmh)
			Expect(err).NotTo(HaveOccurred())

			// Verify annotation was added
			var updatedBMH metal3v1alpha1.BareMetalHost
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			_, exists := updatedBMH.Annotations[BmhRebootAnnotation]
			Expect(exists).To(BeTrue())
		})
	})

	Describe("removeInfraEnvLabel", func() {
		var (
			fakeClient client.Client
			bmh        *metal3v1alpha1.BareMetalHost
			image      *metal3v1alpha1.PreprovisioningImage
		)

		BeforeEach(func() {
			bmh = createBMH("test-bmh", "test-ns", map[string]string{
				BmhInfraEnvLabel: "test-infraenv",
			}, nil, metal3v1alpha1.StateAvailable)

			image = &metal3v1alpha1.PreprovisioningImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-bmh",
					Namespace: "test-ns",
					Labels: map[string]string{
						BmhInfraEnvLabel: "test-infraenv",
					},
				},
			}

			fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(bmh, image).Build()
		})

		It("should remove InfraEnv label from both BMH and PreprovisioningImage", func() {
			name := types.NamespacedName{Name: bmh.Name, Namespace: bmh.Namespace}
			err := removeInfraEnvLabel(ctx, fakeClient, logger, name)
			Expect(err).NotTo(HaveOccurred())

			// Verify label was removed from BMH
			var updatedBMH metal3v1alpha1.BareMetalHost
			err = fakeClient.Get(ctx, name, &updatedBMH)
			Expect(err).NotTo(HaveOccurred())
			_, exists := updatedBMH.Labels[BmhInfraEnvLabel]
			Expect(exists).To(BeFalse())

			// Verify label was removed from PreprovisioningImage
			var updatedImage metal3v1alpha1.PreprovisioningImage
			err = fakeClient.Get(ctx, name, &updatedImage)
			Expect(err).NotTo(HaveOccurred())
			_, exists = updatedImage.Labels[BmhInfraEnvLabel]
			Expect(exists).To(BeFalse())
		})
	})

	Describe("Constants and types", func() {
		It("should have correct constant values", func() {
			Expect(BmhRebootAnnotation).To(Equal("reboot.metal3.io"))
			Expect(BmhNetworkDataPrefx).To(Equal("network-data"))
			Expect(BiosUpdateNeededAnnotation).To(Equal("clcm.openshift.io/bios-update-needed"))
			Expect(FirmwareUpdateNeededAnnotation).To(Equal("clcm.openshift.io/firmware-update-needed"))
			Expect(BmhAllocatedLabel).To(Equal("clcm.openshift.io/allocated"))
			Expect(NodeNameAnnotation).To(Equal("clcm.openshift.io/node-name"))
			Expect(BmhHostMgmtAnnotation).To(Equal("bmac.agent-install.openshift.io/allow-provisioned-host-management"))
			Expect(BmhInfraEnvLabel).To(Equal("infraenvs.agent-install.openshift.io"))
			Expect(SiteConfigOwnedByLabel).To(Equal("siteconfig.open-cluster-management.io/owned-by"))
			Expect(UpdateReasonBIOSSettings).To(Equal("bios-settings-update"))
			Expect(UpdateReasonFirmware).To(Equal("firmware-update"))
			Expect(ValueTrue).To(Equal("true"))
			Expect(MetaTypeLabel).To(Equal("label"))
			Expect(MetaTypeAnnotation).To(Equal("annotation"))
			Expect(OpAdd).To(Equal("add"))
			Expect(OpRemove).To(Equal("remove"))
			Expect(BmhServicingErr).To(Equal("BMH Servicing Error"))
		})
	})

	Describe("bmhBmcInfo and bmhNodeInfo structs", func() {
		It("should create bmhBmcInfo correctly", func() {
			bmcInfo := bmhBmcInfo{
				Address:         "192.168.1.100",
				CredentialsName: "test-credentials",
			}
			Expect(bmcInfo.Address).To(Equal("192.168.1.100"))
			Expect(bmcInfo.CredentialsName).To(Equal("test-credentials"))
		})

		It("should create bmhNodeInfo correctly", func() {
			nodeInfo := bmhNodeInfo{
				ResourcePoolID: "pool1",
				BMC: &bmhBmcInfo{
					Address:         "192.168.1.100",
					CredentialsName: "test-credentials",
				},
				Interfaces: []*pluginsv1alpha1.Interface{
					{
						Name:       "eth0",
						MACAddress: "00:11:22:33:44:55",
						Label:      "mgmt",
					},
				},
			}
			Expect(nodeInfo.ResourcePoolID).To(Equal("pool1"))
			Expect(nodeInfo.BMC.Address).To(Equal("192.168.1.100"))
			Expect(len(nodeInfo.Interfaces)).To(Equal(1))
			Expect(nodeInfo.Interfaces[0].Name).To(Equal("eth0"))
		})
	})
})
