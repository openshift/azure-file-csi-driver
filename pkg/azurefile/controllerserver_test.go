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

package azurefile

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"
	azure2 "github.com/Azure/go-autorest/autorest/azure"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/azurefile-csi-driver/pkg/util"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/fileshareclient/mock_fileshareclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient/mocksubnetclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	auth "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/fileclient/mockfileclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/storageaccountclient/mockstorageaccountclient"
	azure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

var _ = ginkgo.Describe("TestCreateVolume", func() {
	var d *Driver
	var ctrl *gomock.Controller
	stdVolCap := []*csi.VolumeCapability{
		{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	fakeShareQuota := int32(100)
	stdVolSize := int64(5 * 1024 * 1024 * 1024)
	stdCapRange := &csi.CapacityRange{RequiredBytes: stdVolSize}
	zeroCapRange := &csi.CapacityRange{RequiredBytes: int64(0)}
	lessThanPremCapRange := &csi.CapacityRange{RequiredBytes: int64(fakeShareQuota * 1024 * 1024 * 1024)}

	var computeClientFactory *mock_azclient.MockClientFactory
	var mockFileClient *mock_fileshareclient.MockInterface
	ginkgo.BeforeEach(func() {
		stdVolCap = []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		}
		fakeShareQuota = int32(100)
		stdVolSize = int64(5 * 1024 * 1024 * 1024)
		stdCapRange = &csi.CapacityRange{RequiredBytes: stdVolSize}
		zeroCapRange = &csi.CapacityRange{RequiredBytes: int64(0)}
		lessThanPremCapRange = &csi.CapacityRange{RequiredBytes: int64(fakeShareQuota * 1024 * 1024 * 1024)}
		d = NewFakeDriver()
		ctrl = gomock.NewController(ginkgo.GinkgoT())

		computeClientFactory = mock_azclient.NewMockClientFactory(ctrl)
		d.cloud.ComputeClientFactory = computeClientFactory
		mockFileClient = mock_fileshareclient.NewMockInterface(ctrl)
		computeClientFactory.EXPECT().GetFileShareClientForSub(gomock.Any()).Return(mockFileClient, nil).AnyTimes()
	})
	ginkgo.AfterEach(func() {
		ctrl.Finish()
	})
	ginkgo.When("Controller Capability missing", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-cap-missing",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         nil,
			}

			d.Cap = []*csi.ControllerServiceCapability{}

			expectedErr := status.Errorf(codes.InvalidArgument, "CREATE_DELETE_VOLUME")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})

	ginkgo.When("Volume name missing", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.CreateVolumeRequest{
				Name:               "",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         nil,
			}
			expectedErr := status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Volume capabilities missing", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.CreateVolumeRequest{
				Name:          "random-vol-name-vol-cap-missing",
				CapacityRange: stdCapRange,
				Parameters:    nil,
			}
			expectedErr := status.Error(codes.InvalidArgument, "CreateVolume Volume capabilities not valid: CreateVolume Volume capabilities must be provided")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid volume capabilities", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.CreateVolumeRequest{
				Name:          "random-vol-name-vol-cap-invalid",
				CapacityRange: stdCapRange,
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessType: &csi.VolumeCapability_Block{
							Block: &csi.VolumeCapability_BlockVolume{},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
						},
					},
				},
				Parameters: nil,
			}
			expectedErr := status.Error(codes.InvalidArgument, "CreateVolume Volume capabilities not valid: driver does not support block volumes")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Volume lock already present", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         nil,
			}
			locks := newVolumeLocks()
			locks.locks.Insert(req.GetName())
			d.volumeLocks = locks

			expectedErr := status.Error(codes.Aborted, "An operation with the given Volume ID random-vol-name-vol-cap-invalid already exists")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Disabled fsType", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				fsTypeField:     "test_fs",
				secretNameField: "secretname",
				pvcNamespaceKey: "pvcname",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}

			driverOptions := DriverOptions{
				NodeID:               fakeNodeID,
				DriverName:           DefaultDriverName,
				EnableVHDDiskFeature: false,
			}
			d := NewFakeDriverCustomOptions(driverOptions)

			expectedErr := status.Errorf(codes.InvalidArgument, "fsType storage class parameter enables experimental VDH disk feature which is currently disabled, use --enable-vhd driver option to enable it")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid fsType", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				fsTypeField: "test_fs",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}

			driverOptions := DriverOptions{
				NodeID:               fakeNodeID,
				DriverName:           DefaultDriverName,
				EnableVHDDiskFeature: true,
			}
			d := NewFakeDriverCustomOptions(driverOptions)

			expectedErr := status.Errorf(codes.InvalidArgument, "fsType(test_fs) is not supported, supported fsType list: [cifs smb nfs ext4 ext3 ext2 xfs]")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid protocol", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				protocolField: "test_protocol",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "protocol(test_protocol) is not supported, supported protocol list: [smb nfs]")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("nfs protocol only supports premium storage", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				protocolField: "nfs",
				skuNameField:  "Standard_LRS",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-nfs-protocol-standard-sku",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "nfs protocol only supports premium storage, current account type: Standard_LRS")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid accessTier", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				protocolField:   "smb",
				accessTierField: "test_accessTier",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "shareAccessTier(test_accessTier) is not supported, supported ShareAccessTier list: [Cool Hot Premium TransactionOptimized]")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid rootSquashType", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				rootSquashTypeField: "test_rootSquashType",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "rootSquashType(test_rootSquashType) is not supported, supported RootSquashType list: [AllSquash NoRootSquash RootSquash]")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid fsGroupChangePolicy", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				fsGroupChangePolicyField: "test_fsGroupChangePolicy",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "fsGroupChangePolicy(test_fsGroupChangePolicy) is not supported, supported fsGroupChangePolicy list: [None Always OnRootMismatch]")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid shareNamePrefix", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				shareNamePrefixField: "-invalid",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "shareNamePrefix(-invalid) can only contain lowercase letters, numbers, hyphens, and length should be less than 21")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid accountQuota", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				accountQuotaField: "10",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "invalid accountQuota %d in storage class, minimum quota: %d", 10, minimumAccountQuota)
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("invalid tags format to convert to map", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				skuNameField:               "premium",
				resourceGroupField:         "rg",
				tagsField:                  "tags",
				createAccountField:         "true",
				useSecretCacheField:        "true",
				enableLargeFileSharesField: "true",
				pvcNameKey:                 "pvc",
				pvNameKey:                  "pv",
				shareNamePrefixField:       "pre",
				storageEndpointSuffixField: ".core",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			expectedErr := status.Errorf(codes.InvalidArgument, "Tags 'tags' are invalid, the format should like: 'key1=value1,key2=value2'")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("failed to GetStorageAccesskey", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				protocolField:            "nfs",
				networkEndpointTypeField: "privateendpoint",
				useDataPlaneAPIField:     "true",
				vnetResourceGroupField:   "",
				vnetNameField:            "",
				subnetNameField:          "",
			}
			fakeCloud := &azure.Cloud{
				Config: azure.Config{},
				Environment: azure2.Environment{
					StorageEndpointSuffix: "core.windows.net",
				},
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = fakeCloud
			d.volMap = sync.Map{}
			d.volMap.Store("random-vol-name-vol-cap-invalid", "account")

			expectedErr := status.Errorf(codes.Internal, "failed to GetStorageAccesskey on account(account) rg(), error: StorageAccountClient is nil")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid protocol & fsType combination", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				protocolField: "nfs",
				fsTypeField:   "ext4",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}

			driverOptions := DriverOptions{
				NodeID:               fakeNodeID,
				DriverName:           DefaultDriverName,
				EnableVHDDiskFeature: true,
			}
			d := NewFakeDriverCustomOptions(driverOptions)

			expectedErr := status.Errorf(codes.InvalidArgument, "fsType(ext4) is not supported with protocol(nfs)")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("storeAccountKey must set as true in cross subscription", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				subscriptionIDField:              "abc",
				storeAccountKeyField:             "false",
				selectRandomMatchingAccountField: "true",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = &azure.Cloud{
				Config: azure.Config{},
			}

			expectedErr := status.Errorf(codes.InvalidArgument, "resourceGroup must be provided in cross subscription(abc)")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("invalid selectRandomMatchingAccount value", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				selectRandomMatchingAccountField: "invalid",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-selectRandomMatchingAccount-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = &azure.Cloud{
				Config: azure.Config{},
			}

			expectedErr := status.Errorf(codes.InvalidArgument, "invalid selectrandommatchingaccount: invalid in storage class")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("invalid getLatestAccountKey value", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				getLatestAccountKeyField: "invalid",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-getLatestAccountKey-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = &azure.Cloud{
				Config: azure.Config{},
			}

			expectedErr := status.Errorf(codes.InvalidArgument, "invalid getlatestaccountkey: invalid in storage class")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("storageAccount and matchTags conflict", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				storageAccountField: "abc",
				matchTagsField:      "true",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = &azure.Cloud{
				Config: azure.Config{},
			}

			expectedErr := status.Errorf(codes.InvalidArgument, "matchTags must set as false when storageAccount(abc) is provided")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("invalid privateEndpoint and subnetName combination", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				networkEndpointTypeField: "privateendpoint",
				subnetNameField:          "subnet1,subnet2",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "invalid-privateEndpoint-and-subnetName-combination",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = &azure.Cloud{
				Config: azure.Config{},
			}

			expectedErr := status.Errorf(codes.InvalidArgument, "subnetName(subnet1,subnet2) can only contain one subnet for private endpoint")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Failed to update subnet service endpoints", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				protocolField: "nfs",
			}

			fakeCloud := &azure.Cloud{
				Config: azure.Config{
					ResourceGroup: "rg",
					Location:      "loc",
					VnetName:      "fake-vnet",
					SubnetName:    "fake-subnet",
				},
			}
			retErr := retry.NewError(false, fmt.Errorf("the subnet does not exist"))

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = fakeCloud
			mockSubnetClient := mocksubnetclient.NewMockInterface(ctrl)
			fakeCloud.SubnetsClient = mockSubnetClient

			mockSubnetClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return([]network.Subnet{}, retErr).Times(1)

			expectedErr := status.Errorf(codes.Internal, "update service endpoints failed with error: failed to list the subnets under rg rg vnet fake-vnet: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: the subnet does not exist")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Failed with storeAccountKey is not supported for account with shared access key disabled", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{
				skuNameField:              "premium",
				storageAccountTypeField:   "stoacctype",
				locationField:             "loc",
				storageAccountField:       "stoacc",
				resourceGroupField:        "rg",
				shareNameField:            "",
				diskNameField:             "diskname.vhd",
				fsTypeField:               "",
				storeAccountKeyField:      "storeaccountkey",
				secretNamespaceField:      "default",
				mountPermissionsField:     "0755",
				accountQuotaField:         "1000",
				allowSharedKeyAccessField: "false",
			}

			fakeCloud := &azure.Cloud{
				Config: azure.Config{
					ResourceGroup: "rg",
					Location:      "loc",
					VnetName:      "fake-vnet",
					SubnetName:    "fake-subnet",
				},
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-vol-cap-invalid",
				CapacityRange:      stdCapRange,
				VolumeCapabilities: stdVolCap,
				Parameters:         allParam,
			}
			d.cloud = fakeCloud

			expectedErr := status.Errorf(codes.InvalidArgument, "storeAccountKey is not supported for account with shared access key disabled")
			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("fileServicePropertiesCache is nil", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			name := "baz"
			sku := "sku"
			kind := "StorageV2"
			location := "centralus"
			value := ""
			account := storage.Account{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location}
			accounts := []storage.Account{account}
			keys := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}

			allParam := map[string]string{
				skuNameField:                      "premium",
				locationField:                     "loc",
				storageAccountField:               "",
				resourceGroupField:                "rg",
				shareNameField:                    "",
				diskNameField:                     "diskname.vhd",
				fsTypeField:                       "",
				storeAccountKeyField:              "storeaccountkey",
				secretNamespaceField:              "secretnamespace",
				disableDeleteRetentionPolicyField: "true",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-no-valid-key",
				VolumeCapabilities: stdVolCap,
				CapacityRange:      zeroCapRange,
				Parameters:         allParam,
			}

			mockFileClient := mockfileclient.NewMockInterface(ctrl)
			d.cloud.FileClient = mockFileClient

			mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
			d.cloud.StorageAccountClient = mockStorageAccountsClient

			fileServiceProperties := storage.FileServiceProperties{
				FileServicePropertiesProperties: &storage.FileServicePropertiesProperties{},
			}

			mockFileClient.EXPECT().WithSubscriptionID(gomock.Any()).Return(mockFileClient).AnyTimes()
			mockFileClient.EXPECT().CreateFileShare(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storage.FileShare{FileShareProperties: &storage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			mockFileClient.EXPECT().GetServiceProperties(ctx, gomock.Any(), gomock.Any()).Return(fileServiceProperties, nil).AnyTimes()
			mockFileClient.EXPECT().SetServiceProperties(ctx, gomock.Any(), gomock.Any(), gomock.Any()).Return(fileServiceProperties, nil).AnyTimes()

			expectedErr := fmt.Errorf("failed to ensure storage account: fileServicePropertiesCache is nil")

			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(expectedErr.Error()))
		})
	})
	ginkgo.When("No valid key, check all params, with less than min premium volume", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			name := "baz"
			sku := "sku"
			kind := "StorageV2"
			location := "centralus"
			value := ""
			accounts := []storage.Account{
				{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
			}
			keys := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}

			allParam := map[string]string{
				skuNameField:         "premium",
				locationField:        "loc",
				storageAccountField:  "",
				resourceGroupField:   "rg",
				shareNameField:       "",
				diskNameField:        "diskname.vhd",
				fsTypeField:          "",
				storeAccountKeyField: "storeaccountkey",
				secretNamespaceField: "secretnamespace",
			}

			req := &csi.CreateVolumeRequest{
				Name:               "random-vol-name-no-valid-key-check-all-params",
				VolumeCapabilities: stdVolCap,
				CapacityRange:      lessThanPremCapRange,
				Parameters:         allParam,
			}

			mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
			d.cloud.StorageAccountClient = mockStorageAccountsClient

			mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

			expectedErr := fmt.Errorf("no valid keys")

			_, err := d.CreateVolume(ctx, req)
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(expectedErr.Error()))
		})
	})
	ginkgo.When("management client", func() {
		ginkgo.When("Get file share returns error", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location, AccountProperties: &storage.AccountProperties{}},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-get-file-error",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      stdCapRange,
					Parameters:         nil,
				}

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient

				mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, fmt.Errorf("test error")).AnyTimes()

				expectedErr := status.Errorf(codes.Internal, "test error")

				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).To(gomega.Equal(expectedErr))
			})
		})
		ginkgo.When("Create file share error tests", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}

				allParam := map[string]string{
					storageAccountTypeField:           "premium",
					locationField:                     "loc",
					storageAccountField:               "stoacc",
					resourceGroupField:                "rg",
					shareNameField:                    "",
					diskNameField:                     "diskname.vhd",
					fsTypeField:                       "",
					storeAccountKeyField:              "storeaccountkey",
					secretNamespaceField:              "secretnamespace",
					disableDeleteRetentionPolicyField: "true",
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-crete-file-error",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      lessThanPremCapRange,
					Parameters:         allParam,
				}

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient
				d.cloud.CloudProviderBackoff = true
				d.cloud.ResourceRequestBackoff = wait.Backoff{
					Steps: 6,
				}

				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()

				expectedErr := status.Errorf(codes.Internal, "FileShareProperties or FileShareProperties.ShareQuota is nil")

				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).To(gomega.Equal(expectedErr))
			})
		})
		ginkgo.When("existing file share quota is smaller than request quota", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}

				allParam := map[string]string{
					storageAccountTypeField:           "premium",
					locationField:                     "loc",
					storageAccountField:               "stoacc",
					resourceGroupField:                "rg",
					shareNameField:                    "",
					diskNameField:                     "diskname.vhd",
					fsTypeField:                       "",
					storeAccountKeyField:              "storeaccountkey",
					secretNamespaceField:              "secretnamespace",
					disableDeleteRetentionPolicyField: "true",
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-crete-file-error",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      lessThanPremCapRange,
					Parameters:         allParam,
				}

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient
				d.cloud.CloudProviderBackoff = true
				d.cloud.ResourceRequestBackoff = wait.Backoff{
					Steps: 6,
				}

				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: ptr.To(int32(1))}}, nil).AnyTimes()

				expectedErr := status.Errorf(codes.AlreadyExists, "request file share(random-vol-name-crete-file-error) already exists, but its capacity 1 is smaller than 100")
				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).To(gomega.Equal(expectedErr))
			})
		})
		ginkgo.When("Create disk returns error", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				if runtime.GOOS == "windows" {
					ginkgo.Skip("Skipping test on Windows")
				}
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}

				allParam := map[string]string{
					skuNameField:            "premium",
					storageAccountTypeField: "stoacctype",
					locationField:           "loc",
					storageAccountField:     "stoacc",
					resourceGroupField:      "rg",
					fsTypeField:             "ext4",
					storeAccountKeyField:    "storeaccountkey",
					secretNamespaceField:    "default",
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-create-disk-error",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      lessThanPremCapRange,
					Parameters:         allParam,
				}

				driverOptions := DriverOptions{
					NodeID:               fakeNodeID,
					DriverName:           DefaultDriverName,
					EnableVHDDiskFeature: true,
				}
				d := NewFakeDriverCustomOptions(driverOptions)
				d.cloud.ComputeClientFactory = computeClientFactory
				d.cloud.KubeClient = fake.NewSimpleClientset()

				tests := []struct {
					desc          string
					fileSharename string
					expectedErr   error
				}{
					{
						desc:          "File share name empty",
						fileSharename: "",
						expectedErr:   status.Error(codes.Internal, "failed to create VHD disk: NewSharedKeyCredential(stoacc) failed with error: illegal base64 data at input byte 0"),
					},
					{
						desc:          "File share name provided",
						fileSharename: "filesharename",
						expectedErr:   status.Error(codes.Internal, "failed to create VHD disk: NewSharedKeyCredential(stoacc) failed with error: illegal base64 data at input byte 0"),
					},
				}
				for _, test := range tests {
					allParam[shareNameField] = test.fileSharename

					mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
					d.cloud.StorageAccountClient = mockStorageAccountsClient

					mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
					mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
					mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
					mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
					mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()

					_, err := d.CreateVolume(ctx, req)
					gomega.Expect(err).To(gomega.Equal(test.expectedErr))
				}
			})
		})
		ginkgo.When("Valid request", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}

				allParam := map[string]string{
					skuNameField:            "premium",
					storageAccountTypeField: "stoacctype",
					locationField:           "loc",
					storageAccountField:     "stoacc",
					resourceGroupField:      "rg",
					shareNameField:          "",
					diskNameField:           "diskname.vhd",
					fsTypeField:             "",
					storeAccountKeyField:    "storeaccountkey",
					secretNamespaceField:    "default",
					mountPermissionsField:   "0755",
					accountQuotaField:       "1000",
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-valid-request",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      lessThanPremCapRange,
					Parameters:         allParam,
				}

				d.cloud.KubeClient = fake.NewSimpleClientset()

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient

				mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()
				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		})
		ginkgo.When("invalid mountPermissions", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}

				allParam := map[string]string{
					mountPermissionsField: "0abc",
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-valid-request",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      lessThanPremCapRange,
					Parameters:         allParam,
				}

				d.cloud.KubeClient = fake.NewSimpleClientset()

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient

				mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()

				expectedErr := status.Errorf(codes.InvalidArgument, "invalid %s %s in storage class", "mountPermissions", "0abc")
				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).To(gomega.Equal(expectedErr))
			})
		})
		ginkgo.When("invalid parameter", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}

				allParam := map[string]string{
					"invalidparameter": "invalidparameter",
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-valid-request",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      lessThanPremCapRange,
					Parameters:         allParam,
				}

				d.cloud.KubeClient = fake.NewSimpleClientset()

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient

				mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()

				expectedErr := status.Errorf(codes.InvalidArgument, "invalid parameter %q in storage class", "invalidparameter")
				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).To(gomega.Equal(expectedErr))
			})
		})
		ginkgo.When("Account limit exceeded", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "baz"
				sku := "sku"
				kind := "StorageV2"
				location := "centralus"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}, Kind: storage.Kind(kind), Location: &location},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}
				allParam := map[string]string{
					skuNameField:            "premium",
					storageAccountTypeField: "stoacctype",
					locationField:           "loc",
					storageAccountField:     "stoacc",
					resourceGroupField:      "rg",
					shareNameField:          "",
					diskNameField:           "diskname.vhd",
					fsTypeField:             "",
					storeAccountKeyField:    "storeaccountkey",
					secretNamespaceField:    "default",
				}

				req := &csi.CreateVolumeRequest{
					Name:               "random-vol-name-valid-request",
					VolumeCapabilities: stdVolCap,
					CapacityRange:      lessThanPremCapRange,
					Parameters:         allParam,
				}
				d.cloud = azure.GetTestCloud(ctrl)
				d.cloud.KubeClient = fake.NewSimpleClientset()
				d.cloud.ComputeClientFactory = computeClientFactory
				mockTrack1FileClient := mockfileclient.NewMockInterface(ctrl)
				d.cloud.FileClient = mockTrack1FileClient

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient
				tagValue := "TestTagValue"

				first := mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, fmt.Errorf(accountLimitExceedManagementAPI))
				second := mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil)
				gomock.InOrder(first, second)
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storage.Account{Tags: map[string]*string{"TestKey": &tagValue}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()

				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
		})
		ginkgo.When("Premium storage account type (sku) loads from storage account when not given as parameter and share request size is increased to min. size required by premium", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "stoacc"
				sku := "premium"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}
				capRange := &csi.CapacityRange{RequiredBytes: 1024 * 1024 * 1024, LimitBytes: 1024 * 1024 * 1024}

				allParam := map[string]string{
					locationField:         "loc",
					storageAccountField:   "stoacc",
					resourceGroupField:    "rg",
					shareNameField:        "",
					diskNameField:         "diskname.vhd",
					fsTypeField:           "",
					storeAccountKeyField:  "storeaccountkey",
					secretNamespaceField:  "default",
					mountPermissionsField: "0755",
					accountQuotaField:     "1000",
					protocolField:         smb,
				}
				req := &csi.CreateVolumeRequest{
					Name:               "vol-1",
					Parameters:         allParam,
					VolumeCapabilities: stdVolCap,
					CapacityRange:      capRange,
				}

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient

				mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts[0], nil).AnyTimes()

				_, err := d.CreateVolume(ctx, req)

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		})
		ginkgo.When("Premium storage account type (sku) does not load from storage account for size request above min. premium size", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "stoacc"
				sku := "premium"
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}
				capRange := &csi.CapacityRange{RequiredBytes: 1024 * 1024 * 1024 * 100, LimitBytes: 1024 * 1024 * 1024 * 100}

				allParam := map[string]string{
					locationField:         "loc",
					storageAccountField:   "stoacc",
					resourceGroupField:    "rg",
					shareNameField:        "",
					diskNameField:         "diskname.vhd",
					fsTypeField:           "",
					storeAccountKeyField:  "storeaccountkey",
					secretNamespaceField:  "default",
					mountPermissionsField: "0755",
					accountQuotaField:     "1000",
					protocolField:         smb,
				}
				req := &csi.CreateVolumeRequest{
					Name:               "vol-1",
					Parameters:         allParam,
					VolumeCapabilities: stdVolCap,
					CapacityRange:      capRange,
				}

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient

				mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		})
		ginkgo.When("Storage account type (sku) defaults to standard type and share request size is unchanged", func() {
			ginkgo.It("should fail", func(ctx context.Context) {
				name := "stoacc"
				sku := ""
				value := "foo bar"
				accounts := []storage.Account{
					{Name: &name, Sku: &storage.Sku{Name: storage.SkuName(sku)}},
				}
				keys := storage.AccountListKeysResult{
					Keys: &[]storage.AccountKey{
						{Value: &value},
					},
				}
				capRange := &csi.CapacityRange{RequiredBytes: 1024 * 1024 * 1024, LimitBytes: 1024 * 1024 * 1024}

				allParam := map[string]string{
					locationField:         "loc",
					storageAccountField:   "stoacc",
					resourceGroupField:    "rg",
					shareNameField:        "",
					diskNameField:         "diskname.vhd",
					fsTypeField:           "",
					storeAccountKeyField:  "storeaccountkey",
					secretNamespaceField:  "default",
					mountPermissionsField: "0755",
					accountQuotaField:     "1000",
				}
				req := &csi.CreateVolumeRequest{
					Name:               "vol-1",
					Parameters:         allParam,
					VolumeCapabilities: stdVolCap,
					CapacityRange:      capRange,
				}

				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient

				mockFileClient.EXPECT().Create(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: nil}}, nil).AnyTimes()
				mockFileClient.EXPECT().Get(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(keys, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(accounts[0], nil).AnyTimes()

				_, err := d.CreateVolume(ctx, req)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		})
	})
})

var _ = ginkgo.Describe("TestDeleteVolume", func() {
	var ctrl *gomock.Controller
	var d *Driver
	ginkgo.BeforeEach(func() {
		ctrl = gomock.NewController(ginkgo.GinkgoT())
		d = NewFakeDriver()
	})
	ginkgo.AfterEach(func() {
		ctrl.Finish()
	})
	ginkgo.When("Volume ID missing", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.DeleteVolumeRequest{
				Secrets: map[string]string{},
			}

			expectedErr := status.Error(codes.InvalidArgument, "Volume ID missing in request")
			_, err := d.DeleteVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Controller capability missing", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.DeleteVolumeRequest{
				VolumeId: "vol_1-cap-missing",
				Secrets:  map[string]string{},
			}

			d.Cap = []*csi.ControllerServiceCapability{}

			expectedErr := status.Errorf(codes.InvalidArgument, "invalid delete volume request: %v", req)
			_, err := d.DeleteVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid volume ID", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.DeleteVolumeRequest{
				VolumeId: "vol_1",
				Secrets:  map[string]string{},
			}

			d.Cap = []*csi.ControllerServiceCapability{
				{
					Type: &csi.ControllerServiceCapability_Rpc{
						Rpc: &csi.ControllerServiceCapability_RPC{Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME},
					},
				},
			}

			_, err := d.DeleteVolume(ctx, req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
	})
	ginkgo.When("failed to get account info", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.DeleteVolumeRequest{
				VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#diskname.vhd##secret",
				Secrets:  map[string]string{},
			}

			d.Cap = []*csi.ControllerServiceCapability{
				{
					Type: &csi.ControllerServiceCapability_Rpc{
						Rpc: &csi.ControllerServiceCapability_RPC{Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME},
					},
				},
			}
			d.dataPlaneAPIAccountCache, _ = azcache.NewTimedCache(10*time.Minute, func(_ context.Context, _ string) (interface{}, error) { return nil, nil }, false)
			d.dataPlaneAPIAccountCache.Set("f5713de20cde511e8ba4900", "1")

			expectedErr := status.Errorf(codes.NotFound, "get account info from(vol_1#f5713de20cde511e8ba4900#fileshare#diskname.vhd##secret) failed with error: could not get account key from secret(azure-storage-account-f5713de20cde511e8ba4900-secret): KubeClient is nil")
			_, err := d.DeleteVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Delete file share returns error", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.DeleteVolumeRequest{
				VolumeId: "#f5713de20cde511e8ba4900#fileshare#diskname.vhd#",
				Secrets:  map[string]string{},
			}

			mockFileClient := mockfileclient.NewMockInterface(ctrl)

			d.cloud.FileClient = mockFileClient
			mockFileClient.EXPECT().WithSubscriptionID(gomock.Any()).Return(mockFileClient).AnyTimes()
			mockFileClient.EXPECT().DeleteFileShare(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("test error")).Times(1)

			expectedErr := status.Errorf(codes.Internal, "DeleteFileShare fileshare under account(f5713de20cde511e8ba4900) rg() failed with error: test error")
			_, err := d.DeleteVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Valid request", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.DeleteVolumeRequest{
				VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#diskname.vhd#",
				Secrets:  map[string]string{},
			}

			mockFileClient := mockfileclient.NewMockInterface(ctrl)
			d.cloud = azure.GetTestCloud(ctrl)
			d.cloud.FileClient = mockFileClient
			mockFileClient.EXPECT().WithSubscriptionID(gomock.Any()).Return(mockFileClient).AnyTimes()
			mockFileClient.EXPECT().DeleteFileShare(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(1)

			expectedResp := &csi.DeleteSnapshotResponse{}
			resp, err := d.DeleteVolume(ctx, req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp).To(gomega.BeEquivalentTo(expectedResp))
		})
	})
})

var _ = ginkgo.Describe("TestCopyVolume", func() {
	var ctrl *gomock.Controller
	var d *Driver
	stdVolCap := []*csi.VolumeCapability{
		{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	fakeShareQuota := int32(100)
	lessThanPremCapRange := &csi.CapacityRange{RequiredBytes: int64(fakeShareQuota * 1024 * 1024 * 1024)}
	ginkgo.BeforeEach(func() {
		stdVolCap = []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		}
		fakeShareQuota = int32(100)
		lessThanPremCapRange = &csi.CapacityRange{RequiredBytes: int64(fakeShareQuota * 1024 * 1024 * 1024)}
		ctrl = gomock.NewController(ginkgo.GinkgoT())
		d = NewFakeDriver()
	})
	ginkgo.AfterEach(func() {
		ctrl.Finish()
	})
	ginkgo.When("restore volume from volumeSnapshot nfs is not supported", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{}

			volumeSnapshotSource := &csi.VolumeContentSource_SnapshotSource{
				SnapshotId: "unit-test",
			}
			volumeContentSourceSnapshotSource := &csi.VolumeContentSource_Snapshot{
				Snapshot: volumeSnapshotSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceSnapshotSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "random-vol-name-valid-request",
				VolumeCapabilities:  stdVolCap,
				CapacityRange:       lessThanPremCapRange,
				Parameters:          allParam,
				VolumeContentSource: &volumecontensource,
			}

			expectedErr := fmt.Errorf("protocol nfs is not supported for snapshot restore")
			err := d.copyVolume(ctx, req, "", "", []string{}, "", &ShareOptions{Protocol: armstorage.EnabledProtocolsNFS}, nil, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("restore volume from volumeSnapshot not found", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{}

			volumeSnapshotSource := &csi.VolumeContentSource_SnapshotSource{
				SnapshotId: "unit-test",
			}
			volumeContentSourceSnapshotSource := &csi.VolumeContentSource_Snapshot{
				Snapshot: volumeSnapshotSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceSnapshotSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "random-vol-name-valid-request",
				VolumeCapabilities:  stdVolCap,
				CapacityRange:       lessThanPremCapRange,
				Parameters:          allParam,
				VolumeContentSource: &volumecontensource,
			}

			expectedErr := status.Errorf(codes.NotFound, "error parsing volume id: \"unit-test\", should at least contain two #")
			err := d.copyVolume(ctx, req, "", "", []string{}, "", &ShareOptions{Name: "dstFileshare"}, nil, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("restore volume from volumeSnapshot src fileshare is empty", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{}

			volumeSnapshotSource := &csi.VolumeContentSource_SnapshotSource{
				SnapshotId: "rg#unit-test###",
			}
			volumeContentSourceSnapshotSource := &csi.VolumeContentSource_Snapshot{
				Snapshot: volumeSnapshotSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceSnapshotSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "random-vol-name-valid-request",
				VolumeCapabilities:  stdVolCap,
				CapacityRange:       lessThanPremCapRange,
				Parameters:          allParam,
				VolumeContentSource: &volumecontensource,
			}

			expectedErr := fmt.Errorf("one or more of srcAccountName(unit-test), srcFileShareName(), dstFileShareName(dstFileshare) are empty")
			err := d.copyVolume(ctx, req, "", "", []string{}, "", &ShareOptions{Name: "dstFileshare"}, nil, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("copy volume nfs is not supported", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{}

			volumeSource := &csi.VolumeContentSource_VolumeSource{
				VolumeId: "unit-test",
			}
			volumeContentSourceVolumeSource := &csi.VolumeContentSource_Volume{
				Volume: volumeSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceVolumeSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "random-vol-name-valid-request",
				VolumeCapabilities:  stdVolCap,
				CapacityRange:       lessThanPremCapRange,
				Parameters:          allParam,
				VolumeContentSource: &volumecontensource,
			}

			expectedErr := fmt.Errorf("protocol nfs is not supported for volume cloning")
			err := d.copyVolume(ctx, req, "", "", []string{}, "", &ShareOptions{Protocol: armstorage.EnabledProtocolsNFS}, nil, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("copy volume from volume not found", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{}

			volumeSource := &csi.VolumeContentSource_VolumeSource{
				VolumeId: "unit-test",
			}
			volumeContentSourceVolumeSource := &csi.VolumeContentSource_Volume{
				Volume: volumeSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceVolumeSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "random-vol-name-valid-request",
				VolumeCapabilities:  stdVolCap,
				CapacityRange:       lessThanPremCapRange,
				Parameters:          allParam,
				VolumeContentSource: &volumecontensource,
			}

			expectedErr := status.Errorf(codes.NotFound, "error parsing volume id: \"unit-test\", should at least contain two #")
			err := d.copyVolume(ctx, req, "", "", []string{}, "", &ShareOptions{Name: "dstFileshare"}, nil, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("src fileshare is empty", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{}

			volumeSource := &csi.VolumeContentSource_VolumeSource{
				VolumeId: "rg#unit-test##",
			}
			volumeContentSourceVolumeSource := &csi.VolumeContentSource_Volume{
				Volume: volumeSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceVolumeSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "random-vol-name-valid-request",
				VolumeCapabilities:  stdVolCap,
				CapacityRange:       lessThanPremCapRange,
				Parameters:          allParam,
				VolumeContentSource: &volumecontensource,
			}

			expectedErr := fmt.Errorf("one or more of srcAccountName(unit-test), srcFileShareName(), dstFileShareName(dstFileshare) are empty")
			err := d.copyVolume(ctx, req, "", "", []string{}, "", &ShareOptions{Name: "dstFileshare"}, nil, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("dst fileshare is empty", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			allParam := map[string]string{}

			volumeSource := &csi.VolumeContentSource_VolumeSource{
				VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#",
			}
			volumeContentSourceVolumeSource := &csi.VolumeContentSource_Volume{
				Volume: volumeSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceVolumeSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "random-vol-name-valid-request",
				VolumeCapabilities:  stdVolCap,
				CapacityRange:       lessThanPremCapRange,
				Parameters:          allParam,
				VolumeContentSource: &volumecontensource,
			}

			expectedErr := fmt.Errorf("one or more of srcAccountName(f5713de20cde511e8ba4900), srcFileShareName(fileshare), dstFileShareName() are empty")
			err := d.copyVolume(ctx, req, "", "", []string{}, "", &ShareOptions{}, nil, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("azcopy job is in progress", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			accountOptions := azure.AccountOptions{}
			mp := map[string]string{}

			volumeSource := &csi.VolumeContentSource_VolumeSource{
				VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#",
			}
			volumeContentSourceVolumeSource := &csi.VolumeContentSource_Volume{
				Volume: volumeSource,
			}
			volumecontensource := csi.VolumeContentSource{
				Type: volumeContentSourceVolumeSource,
			}

			req := &csi.CreateVolumeRequest{
				Name:                "unit-test",
				VolumeCapabilities:  stdVolCap,
				Parameters:          mp,
				VolumeContentSource: &volumecontensource,
			}

			m := util.NewMockEXEC(ctrl)
			listStr1 := "JobId: ed1c3833-eaff-fe42-71d7-513fb065a9d9\nStart Time: Monday, 07-Aug-23 03:29:54 UTC\nStatus: InProgress\nCommand: copy https://{accountName}.file.core.windows.net/{srcFileshare}{SAStoken} https://{accountName}.file.core.windows.net/{dstFileshare}{SAStoken} --recursive --check-length=false"
			m.EXPECT().RunCommand(gomock.Eq("azcopy jobs list | grep dstFileshare -B 3"), gomock.Any()).Return(listStr1, nil).AnyTimes()
			m.EXPECT().RunCommand(gomock.Not("azcopy jobs list | grep dstFileshare -B 3"), gomock.Any()).Return("Percent Complete (approx): 50.0", nil).AnyTimes()

			d.azcopy.ExecCmd = m
			d.waitForAzCopyTimeoutMinutes = 1

			err := d.copyVolume(ctx, req, "", "sastoken", []string{}, "", &ShareOptions{Name: "dstFileshare"}, &accountOptions, "core.windows.net")
			gomega.Expect(err).To(gomega.Equal(wait.ErrWaitTimeout))
		})
	})
})

var _ = ginkgo.Describe("ControllerGetVolume", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {
			d := NewFakeDriver()
			req := csi.ControllerGetVolumeRequest{}
			resp, err := d.ControllerGetVolume(context.Background(), &req)
			gomega.Expect(resp).To(gomega.BeNil())
			gomega.Expect(err).To(gomega.Equal(status.Error(codes.Unimplemented, "")))
		})
	})
})

var _ = ginkgo.Describe("ControllerGetCapabilities", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {

			d := NewFakeDriver()
			controlCap := []*csi.ControllerServiceCapability{
				{
					Type: &csi.ControllerServiceCapability_Rpc{},
				},
			}
			d.Cap = controlCap
			req := csi.ControllerGetCapabilitiesRequest{}
			resp, err := d.ControllerGetCapabilities(context.Background(), &req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp).NotTo(gomega.BeNil())
			gomega.Expect(resp.Capabilities).To(gomega.Equal(controlCap))
		})
	})
})

var _ = ginkgo.DescribeTable("ValidateVolumeCapabilities", func(
	req *csi.ValidateVolumeCapabilitiesRequest,
	expectedErr error,
	mockedFileShareErr error,
) {
	ctrl := gomock.NewController(ginkgo.GinkgoT())
	defer ctrl.Finish()
	d := NewFakeDriver()
	d.cloud = &azure.Cloud{}
	computeClientFactory := mock_azclient.NewMockClientFactory(ctrl)
	d.cloud.ComputeClientFactory = computeClientFactory
	mockFileClient := mock_fileshareclient.NewMockInterface(ctrl)
	computeClientFactory.EXPECT().GetFileShareClientForSub(gomock.Any()).Return(mockFileClient, nil).AnyTimes()
	value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
	key := storage.AccountListKeysResult{
		Keys: &[]storage.AccountKey{
			{Value: &value},
		},
	}
	clientSet := fake.NewSimpleClientset()

	fakeShareQuota := int32(100)
	mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
	d.cloud.StorageAccountClient = mockStorageAccountsClient
	d.cloud.KubeClient = clientSet
	d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
	mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(key, nil).AnyTimes()
	mockFileClient.EXPECT().Get(context.Background(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: &fakeShareQuota}}, mockedFileShareErr).AnyTimes()

	_, err := d.ValidateVolumeCapabilities(context.Background(), req)
	if expectedErr == nil {
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	} else {
		gomega.Expect(err).To(gomega.Equal(expectedErr))
	}
},
	ginkgo.Entry("Volume ID missing",
		&csi.ValidateVolumeCapabilitiesRequest{},
		status.Error(codes.InvalidArgument, "Volume ID not provided"),
		nil,
	),
	ginkgo.Entry("Volume capabilities missing",
		&csi.ValidateVolumeCapabilitiesRequest{VolumeId: "vol_1"},
		status.Error(codes.InvalidArgument, "Volume capabilities not provided"),
		nil,
	),
	ginkgo.Entry("Volume ID not valid",
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "vol_1",
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
			},
		},
		status.Errorf(codes.NotFound, "get account info from(vol_1) failed with error: <nil>"),
		nil,
	),
	ginkgo.Entry("Check file share exists errors",
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#",
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
			},
		},
		status.Errorf(codes.Internal, "error checking if volume(vol_1#f5713de20cde511e8ba4900#fileshare#) exists: test error"),
		fmt.Errorf("test error"),
	),
	ginkgo.Entry("Valid request disk name is empty",
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#",
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
			},
		},
		nil,
		nil,
	),
	ginkgo.Entry("Valid request volume capability is multi node single writer",
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#diskname.vhd#",
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
					},
				},
			},
		},
		nil,
		nil,
	),
	ginkgo.Entry("Valid request",
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#diskname.vhd#",
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
			},
		},
		nil,
		nil,
	),
	ginkgo.Entry("Resource group empty",
		&csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "vol_1#f5713de20cde511e8ba4900#fileshare#diskname.vhd#",
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
			},
			VolumeContext: map[string]string{
				shareNameField: "sharename",
				diskNameField:  "diskname.vhd",
			},
		},
		nil,
		nil,
	),
)
var _ = ginkgo.Describe("CreateSnapshot", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {

			d := NewFakeDriver()

			tests := []struct {
				desc        string
				req         *csi.CreateSnapshotRequest
				expectedErr error
			}{
				{
					desc:        "Snapshot name missing",
					req:         &csi.CreateSnapshotRequest{},
					expectedErr: status.Error(codes.InvalidArgument, "Snapshot name must be provided"),
				},
				{
					desc: "Source volume ID",
					req: &csi.CreateSnapshotRequest{
						Name: "snapname",
					},
					expectedErr: status.Error(codes.InvalidArgument, "CreateSnapshot Source Volume ID must be provided"),
				},
				{
					desc: "Invalid volume ID",
					req: &csi.CreateSnapshotRequest{
						SourceVolumeId: "vol_1",
						Name:           "snapname",
					},
					expectedErr: status.Errorf(codes.Internal, `GetFileShareInfo(vol_1) failed with error: error parsing volume id: "vol_1", should at least contain two #`),
				},
			}

			for _, test := range tests {
				_, err := d.CreateSnapshot(context.Background(), test.req)
				gomega.Expect(err).To(gomega.Equal(test.expectedErr))
			}
		})
	})
})
var _ = ginkgo.Describe("DeleteSnapshot", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {

			d := NewFakeDriver()

			validSecret := map[string]string{}
			ctrl := gomock.NewController(ginkgo.GinkgoT())

			defer ctrl.Finish()
			value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
			key := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}
			clientSet := fake.NewSimpleClientset()

			tests := []struct {
				desc        string
				req         *csi.DeleteSnapshotRequest
				expectedErr error
			}{
				{
					desc:        "Snapshot name missing",
					req:         &csi.DeleteSnapshotRequest{},
					expectedErr: status.Error(codes.InvalidArgument, "Snapshot ID must be provided"),
				},
				{
					desc: "Invalid volume ID",
					req: &csi.DeleteSnapshotRequest{
						SnapshotId: "vol_1#",
					},
					expectedErr: nil,
				},
				{
					desc: "Invalid volume ID for snapshot name",
					req: &csi.DeleteSnapshotRequest{
						SnapshotId: "vol_1##",
						Secrets:    validSecret,
					},
					expectedErr: nil,
				},
				{
					desc: "Invalid Snapshot ID",
					req: &csi.DeleteSnapshotRequest{
						SnapshotId: "testrg#testAccount#testFileShare#testuuid",
						Secrets:    map[string]string{"accountName": "TestAccountName", "accountKey": base64.StdEncoding.EncodeToString([]byte("TestAccountKey"))},
					},
					expectedErr: status.Error(codes.Internal, "failed to get snapshot name with (testrg#testAccount#testFileShare#testuuid): error parsing volume id: \"testrg#testAccount#testFileShare#testuuid\", should at least contain four #"),
				},
			}

			for _, test := range tests {
				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient
				d.cloud.KubeClient = clientSet
				d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "vol_1", gomock.Any()).Return(key, nil).AnyTimes()

				_, err := d.DeleteSnapshot(context.Background(), test.req)
				if test.expectedErr == nil {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				} else {
					gomega.Expect(err).To(gomega.Equal(test.expectedErr))
				}
			}
		})
	})
})

var _ = ginkgo.Describe("TestControllerExpandVolume", func() {
	stdVolSize := int64(5 * 1024 * 1024 * 1024)
	stdCapRange := &csi.CapacityRange{RequiredBytes: stdVolSize}
	var ctrl *gomock.Controller
	var d *Driver
	ginkgo.BeforeEach(func() {
		stdVolSize = int64(5 * 1024 * 1024 * 1024)
		stdCapRange = &csi.CapacityRange{RequiredBytes: stdVolSize}
		ctrl = gomock.NewController(ginkgo.GinkgoT())
		d = NewFakeDriver()
	})
	ginkgo.AfterEach(func() {
		ctrl.Finish()
	})
	ginkgo.When("Volume ID missing", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.ControllerExpandVolumeRequest{}

			expectedErr := status.Error(codes.InvalidArgument, "Volume ID missing in request")
			_, err := d.ControllerExpandVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Volume Capacity range missing", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.ControllerExpandVolumeRequest{
				VolumeId: "vol_1",
			}

			d.Cap = []*csi.ControllerServiceCapability{}

			expectedErr := status.Error(codes.InvalidArgument, "volume capacity range missing in request")
			_, err := d.ControllerExpandVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Invalid Volume ID", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			req := &csi.ControllerExpandVolumeRequest{
				VolumeId:      "vol_1",
				CapacityRange: stdCapRange,
			}

			expectedErr := status.Errorf(codes.InvalidArgument, "GetFileShareInfo(vol_1) failed with error: error parsing volume id: \"vol_1\", should at least contain two #")
			_, err := d.ControllerExpandVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("Disk name not empty", func() {
		ginkgo.It("should fail", func(ctx context.Context) {

			value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
			key := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}
			clientSet := fake.NewSimpleClientset()
			req := &csi.ControllerExpandVolumeRequest{
				VolumeId:      "vol_1#f5713de20cde511e8ba4900#filename#diskname.vhd#",
				CapacityRange: stdCapRange,
			}

			mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
			d.cloud.StorageAccountClient = mockStorageAccountsClient
			d.cloud.KubeClient = clientSet
			d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
			mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "vol_1", gomock.Any()).Return(key, nil).AnyTimes()

			expectErr := status.Error(codes.Unimplemented, "vhd disk volume(vol_1#f5713de20cde511e8ba4900#filename#diskname.vhd#, diskName:diskname.vhd) is not supported on ControllerExpandVolume")
			_, err := d.ControllerExpandVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectErr))
		})
	})
	ginkgo.When("Resize file share returns error", func() {
		ginkgo.It("should fail", func(ctx context.Context) {

			value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
			key := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}
			clientSet := fake.NewSimpleClientset()
			req := &csi.ControllerExpandVolumeRequest{
				VolumeId:      "vol_1#f5713de20cde511e8ba4900#filename#",
				CapacityRange: stdCapRange,
			}

			mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
			d.cloud.StorageAccountClient = mockStorageAccountsClient
			d.cloud.KubeClient = clientSet
			d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
			mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "vol_1", gomock.Any()).Return(key, nil).AnyTimes()
			mockFileClient := mockfileclient.NewMockInterface(ctrl)
			mockFileClient.EXPECT().WithSubscriptionID(gomock.Any()).Return(mockFileClient).AnyTimes()
			mockFileClient.EXPECT().ResizeFileShare(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("test error")).AnyTimes()
			d.cloud.FileClient = mockFileClient

			expectErr := status.Errorf(codes.Internal, "expand volume error: test error")
			_, err := d.ControllerExpandVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectErr))
		})
	})
	ginkgo.When("get account info failed", func() {
		ginkgo.It("should fail", func(ctx context.Context) {

			d.cloud = &azure.Cloud{
				Config: azure.Config{
					ResourceGroup: "vol_2",
				},
			}
			d.dataPlaneAPIAccountCache, _ = azcache.NewTimedCache(10*time.Minute, func(_ context.Context, _ string) (interface{}, error) { return nil, nil }, false)
			d.dataPlaneAPIAccountCache.Set("f5713de20cde511e8ba4900", "1")

			value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
			key := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}
			clientSet := fake.NewSimpleClientset()
			req := &csi.ControllerExpandVolumeRequest{
				VolumeId:      "#f5713de20cde511e8ba4900#filename##secret",
				CapacityRange: stdCapRange,
			}

			mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
			d.cloud.StorageAccountClient = mockStorageAccountsClient
			d.cloud.KubeClient = clientSet
			d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
			mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "vol_2", gomock.Any()).Return(key, &retry.Error{HTTPStatusCode: http.StatusBadGateway, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

			expectErr := status.Error(codes.NotFound, "get account info from(#f5713de20cde511e8ba4900#filename##secret) failed with error: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 502, RawError: instance not found")
			_, err := d.ControllerExpandVolume(ctx, req)
			gomega.Expect(err).To(gomega.Equal(expectErr))

		})
	})
	ginkgo.When("Valid request", func() {
		ginkgo.It("should fail", func(ctx context.Context) {

			value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
			key := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}
			clientSet := fake.NewSimpleClientset()
			req := &csi.ControllerExpandVolumeRequest{
				VolumeId:      "capz-d18sqm#f25f6e46c62274a4a8e433a#pvc-66ced8fb-a027-4eb6-87ca-e720ff36f683#pvc-66ced8fb-a027-4eb6-87ca-e720ff36f683#azurefile-2546",
				CapacityRange: stdCapRange,
			}

			mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
			d.cloud.StorageAccountClient = mockStorageAccountsClient
			d.cloud.KubeClient = clientSet
			d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
			mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "capz-d18sqm", gomock.Any()).Return(key, nil).AnyTimes()
			mockFileClient := mockfileclient.NewMockInterface(ctrl)
			mockFileClient.EXPECT().WithSubscriptionID(gomock.Any()).Return(mockFileClient).AnyTimes()
			mockFileClient.EXPECT().ResizeFileShare(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			shareQuota := int32(0)
			mockFileClient.EXPECT().GetFileShare(ctx, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storage.FileShare{FileShareProperties: &storage.FileShareProperties{ShareQuota: &shareQuota}}, nil).AnyTimes()
			d.cloud.FileClient = mockFileClient

			expectedResp := &csi.ControllerExpandVolumeResponse{CapacityBytes: stdVolSize}
			resp, err := d.ControllerExpandVolume(ctx, req)
			if !(reflect.DeepEqual(err, nil) && reflect.DeepEqual(resp, expectedResp)) {

				ginkgo.GinkgoT().Errorf("Expected response: %v received response: %v expected error: %v received error: %v", expectedResp, resp, nil, err)
			}
		})
	})
})

var _ = ginkgo.Describe("GetShareURL", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {

			d := NewFakeDriver()

			validSecret := map[string]string{}

			ctrl := gomock.NewController(ginkgo.GinkgoT())

			defer ctrl.Finish()
			value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
			key := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}
			clientSet := fake.NewSimpleClientset()
			tests := []struct {
				desc           string
				sourceVolumeID string
				expectedErr    error
			}{
				{
					desc:           "Volume ID error",
					sourceVolumeID: "vol_1",
					expectedErr:    fmt.Errorf("failed to get file share from vol_1"),
				},
				{
					desc:           "Volume ID error2",
					sourceVolumeID: "vol_1###",
					expectedErr:    fmt.Errorf("failed to get file share from vol_1###"),
				},
				{
					desc:           "Valid request",
					sourceVolumeID: "rg#accountname#fileshare#",
					expectedErr:    nil,
				},
			}

			for _, test := range tests {
				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient
				d.cloud.KubeClient = clientSet
				d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "rg", gomock.Any()).Return(key, nil).AnyTimes()
				_, err := d.getShareURL(context.Background(), test.sourceVolumeID, validSecret)
				if test.expectedErr == nil {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				} else {
					gomega.Expect(err).To(gomega.Equal(test.expectedErr))
				}
			}
		})
	})
})

var _ = ginkgo.DescribeTable("GetServiceURL", func(sourceVolumeID string, key storage.AccountListKeysResult, expectedErr error) {
	d := NewFakeDriver()
	validSecret := map[string]string{}

	ctrl := gomock.NewController(ginkgo.GinkgoT())
	defer ctrl.Finish()

	clientSet := fake.NewSimpleClientset()
	mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
	d.cloud.StorageAccountClient = mockStorageAccountsClient
	d.cloud.KubeClient = clientSet
	d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
	mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "vol_1", gomock.Any()).Return(key, nil).AnyTimes()

	_, _, err := d.getServiceURL(context.Background(), sourceVolumeID, validSecret)
	if expectedErr == nil {
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	} else {
		gomega.Expect(err).To(gomega.Equal(expectedErr))
	}
},
	ginkgo.Entry("Invalid volume ID", "vol_1", storage.AccountListKeysResult{
		Keys: &[]storage.AccountKey{
			{Value: to.Ptr(base64.StdEncoding.EncodeToString([]byte("acc_key")))},
		},
	}, nil),
	ginkgo.Entry("Invalid Key",
		"vol_1##",
		storage.AccountListKeysResult{
			Keys: &[]storage.AccountKey{
				{Value: to.Ptr("acc_key")},
			},
		},
		nil,
	),
	ginkgo.Entry("Invalid URL",
		"vol_1#^f5713de20cde511e8ba4900#",
		storage.AccountListKeysResult{
			Keys: &[]storage.AccountKey{
				{Value: to.Ptr(base64.StdEncoding.EncodeToString([]byte("acc_key")))},
			},
		},
		&url.Error{Op: "parse", URL: "https://^f5713de20cde511e8ba4900.file.abc", Err: url.InvalidHostError("^")},
	),
	ginkgo.Entry("Valid call",
		"vol_1##",
		storage.AccountListKeysResult{
			Keys: &[]storage.AccountKey{
				{Value: to.Ptr(base64.StdEncoding.EncodeToString([]byte("acc_key")))},
			},
		},
		nil,
	),
)

var _ = ginkgo.Describe("SnapshotExists", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {
			d := NewFakeDriver()
			validSecret := map[string]string{}

			ctrl := gomock.NewController(ginkgo.GinkgoT())
			defer ctrl.Finish()
			value := base64.StdEncoding.EncodeToString([]byte("acc_key"))
			validKey := storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			}
			clientSet := fake.NewSimpleClientset()
			tests := []struct {
				desc           string
				sourceVolumeID string
				key            storage.AccountListKeysResult
				secret         map[string]string
				expectedErr    error
			}{
				{
					desc:           "Invalid volume ID with data plane api",
					sourceVolumeID: "vol_1",
					key:            validKey,
					secret:         map[string]string{"accountName": "TestAccountName", "accountKey": base64.StdEncoding.EncodeToString([]byte("TestAccountKey"))},
					expectedErr:    fmt.Errorf("file share is empty after parsing sourceVolumeID: vol_1"),
				},
				{
					desc:           "Invalid volume ID with management api",
					sourceVolumeID: "vol_1",
					key:            validKey,
					secret:         validSecret,
					expectedErr:    fmt.Errorf("error parsing volume id: %q, should at least contain two #", "vol_1"),
				},
			}

			for _, test := range tests {
				mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
				d.cloud.StorageAccountClient = mockStorageAccountsClient
				d.cloud.KubeClient = clientSet
				d.cloud.Environment = azure2.Environment{StorageEndpointSuffix: "abc"}
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), "", gomock.Any()).Return(test.key, nil).AnyTimes()

				_, _, _, _, err := d.snapshotExists(context.Background(), test.sourceVolumeID, "sname", test.secret, false)
				gomega.Expect(err).To(gomega.Equal(test.expectedErr))
			}
		})
	})
})

var _ = ginkgo.Describe("GetCapacity", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {

			d := NewFakeDriver()
			req := csi.GetCapacityRequest{}
			resp, err := d.GetCapacity(context.Background(), &req)
			gomega.Expect(resp).To(gomega.BeNil())
			gomega.Expect(err).To(gomega.Equal(status.Error(codes.Unimplemented, "")))
		})
	})
})

var _ = ginkgo.Describe("ListVolumes", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {
			d := NewFakeDriver()
			req := csi.ListVolumesRequest{}
			resp, err := d.ListVolumes(context.Background(), &req)
			gomega.Expect(resp).To(gomega.BeNil())
			gomega.Expect(err).To(gomega.Equal(status.Error(codes.Unimplemented, "")))
		})
	})
})

var _ = ginkgo.Describe("ListSnapshots", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {
			d := NewFakeDriver()
			req := csi.ListSnapshotsRequest{}
			resp, err := d.ListSnapshots(context.Background(), &req)
			gomega.Expect(resp).To(gomega.BeNil())
			gomega.Expect(err).To(gomega.Equal(status.Error(codes.Unimplemented, "")))
		})
	})
})

var _ = ginkgo.Describe("SetAzureCredentials", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {
			d := NewFakeDriver()
			d.cloud = &azure.Cloud{
				Config: azure.Config{
					ResourceGroup: "rg",
					Location:      "loc",
					VnetName:      "fake-vnet",
					SubnetName:    "fake-subnet",
				},
			}
			fakeClient := fake.NewSimpleClientset()

			tests := []struct {
				desc            string
				kubeClient      kubernetes.Interface
				accountName     string
				accountKey      string
				secretName      string
				secretNamespace string
				expectedName    string
				expectedErr     error
			}{
				{
					desc:        "[failure] accountName is nil",
					kubeClient:  fakeClient,
					expectedErr: fmt.Errorf("the account info is not enough, accountName(), accountKey()"),
				},
				{
					desc:        "[failure] accountKey is nil",
					kubeClient:  fakeClient,
					accountName: "testName",
					accountKey:  "",
					expectedErr: fmt.Errorf("the account info is not enough, accountName(testName), accountKey()"),
				},
				{
					desc:        "[success] kubeClient is nil",
					kubeClient:  nil,
					expectedErr: nil,
				},
				{
					desc:         "[success] normal scenario",
					kubeClient:   fakeClient,
					accountName:  "testName",
					accountKey:   "testKey",
					expectedName: "azure-storage-account-testName-secret",
					expectedErr:  nil,
				},
				{
					desc:         "[success] already exist",
					kubeClient:   fakeClient,
					accountName:  "testName",
					accountKey:   "testKey",
					expectedName: "azure-storage-account-testName-secret",
					expectedErr:  nil,
				},
				{
					desc:            "[success] normal scenario using secretName",
					kubeClient:      fakeClient,
					accountName:     "testName",
					accountKey:      "testKey",
					secretName:      "secretName",
					secretNamespace: "secretNamespace",
					expectedName:    "secretName",
					expectedErr:     nil,
				},
			}

			for _, test := range tests {
				d.cloud.KubeClient = test.kubeClient
				result, err := d.SetAzureCredentials(context.Background(), test.accountName, test.accountKey, test.secretName, test.secretNamespace)
				gomega.Expect(result).To(gomega.Equal(test.expectedName))
				if test.expectedErr == nil {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				} else {
					gomega.Expect(err).To(gomega.Equal(test.expectedErr))
				}
			}
		})
	})
})

var _ = ginkgo.Describe("GenerateSASToken", func() {
	ginkgo.When("test", func() {
		ginkgo.It("should work", func(_ context.Context) {
			d := NewFakeDriver()
			storageEndpointSuffix := "core.windows.net"
			tests := []struct {
				name        string
				accountName string
				accountKey  string
				want        string
				expectedErr error
			}{
				{
					name:        "accountName nil",
					accountName: "",
					accountKey:  "",
					want:        "se=",
					expectedErr: nil,
				},
				{
					name:        "account key illegal",
					accountName: "unit-test",
					accountKey:  "fakeValue",
					want:        "",
					expectedErr: status.Errorf(codes.Internal, "failed to generate sas token in creating new shared key credential, accountName: %s, err: %s", "unit-test", "decode account key: illegal base64 data at input byte 8"),
				},
			}
			for _, tt := range tests {
				sas, err := d.generateSASToken(context.Background(), tt.accountName, tt.accountKey, storageEndpointSuffix, 30)
				if tt.expectedErr == nil {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				} else {
					gomega.Expect(err).To(gomega.Equal(tt.expectedErr))
				}
				gomega.Expect(sas).To(gomega.ContainSubstring(tt.want))
			}
		})
	})
})

var _ = ginkgo.Describe("TestAuthorizeAzcopyWithIdentity", func() {
	var d *Driver
	ginkgo.BeforeEach(func() {
		d = NewFakeDriver()
	})
	ginkgo.When("use service principal to authorize azcopy", func() {
		ginkgo.It("should fail", func(_ context.Context) {
			d.cloud = &azure.Cloud{
				Config: azure.Config{
					AzureClientConfig: config.AzureClientConfig{
						ARMClientConfig: azclient.ARMClientConfig{
							TenantID: "TenantID",
						},
						AzureAuthConfig: azclient.AzureAuthConfig{
							AADClientID:     "AADClientID",
							AADClientSecret: "AADClientSecret",
						},
					},
				},
			}
			expectedAuthAzcopyEnv := []string{
				fmt.Sprintf(azcopyAutoLoginType + "=SPN"),
				fmt.Sprintf(azcopySPAApplicationID + "=AADClientID"),
				fmt.Sprintf(azcopySPAClientSecret + "=AADClientSecret"),
				fmt.Sprintf(azcopyTenantID + "=TenantID"),
			}
			authAzcopyEnv, err := d.authorizeAzcopyWithIdentity()
			if !reflect.DeepEqual(authAzcopyEnv, expectedAuthAzcopyEnv) || err != nil {
				ginkgo.GinkgoT().Errorf("Unexpected authAzcopyEnv: %v, Unexpected error: %v", authAzcopyEnv, err)
			}
		})
	})
	ginkgo.When("use service principal to authorize azcopy but client id is empty", func() {
		ginkgo.It("should fail", func(_ context.Context) {
			d.cloud = &azure.Cloud{
				Config: azure.Config{
					AzureClientConfig: config.AzureClientConfig{
						ARMClientConfig: azclient.ARMClientConfig{
							TenantID: "TenantID",
						},
						AzureAuthConfig: azclient.AzureAuthConfig{
							AADClientSecret: "AADClientSecret",
						},
					},
				},
			}
			expectedAuthAzcopyEnv := []string{}
			expectedErr := fmt.Errorf("AADClientID and TenantID must be set when use service principal")
			authAzcopyEnv, err := d.authorizeAzcopyWithIdentity()
			gomega.Expect(authAzcopyEnv).To(gomega.Equal(expectedAuthAzcopyEnv))
			gomega.Expect(err).To(gomega.Equal(expectedErr))
		})
	})
	ginkgo.When("use user assigned managed identity to authorize azcopy", func() {
		ginkgo.It("should fail", func(_ context.Context) {
			d.cloud = &azure.Cloud{
				Config: azure.Config{
					AzureClientConfig: config.AzureClientConfig{
						AzureAuthConfig: azclient.AzureAuthConfig{
							UseManagedIdentityExtension: true,
							UserAssignedIdentityID:      "UserAssignedIdentityID",
						},
					},
				},
			}
			expectedAuthAzcopyEnv := []string{
				fmt.Sprintf(azcopyAutoLoginType + "=MSI"),
				fmt.Sprintf(azcopyMSIClientID + "=UserAssignedIdentityID"),
			}
			var expected error
			authAzcopyEnv, err := d.authorizeAzcopyWithIdentity()
			if !reflect.DeepEqual(authAzcopyEnv, expectedAuthAzcopyEnv) || !reflect.DeepEqual(err, expected) {
				ginkgo.GinkgoT().Errorf("Unexpected authAzcopyEnv: %v, Unexpected error: %v", authAzcopyEnv, err)
			}
		})
	})
	ginkgo.When("use system assigned managed identity to authorize azcopy", func() {
		ginkgo.It("should fail", func(_ context.Context) {
			d.cloud = &azure.Cloud{
				Config: azure.Config{
					AzureClientConfig: auth.AzureClientConfig{
						AzureAuthConfig: azclient.AzureAuthConfig{
							UseManagedIdentityExtension: true,
						},
					},
				},
			}
			expectedAuthAzcopyEnv := []string{
				fmt.Sprintf(azcopyAutoLoginType + "=MSI"),
			}
			var expected error
			authAzcopyEnv, err := d.authorizeAzcopyWithIdentity()
			if !reflect.DeepEqual(authAzcopyEnv, expectedAuthAzcopyEnv) || !reflect.DeepEqual(err, expected) {

				ginkgo.GinkgoT().Errorf("Unexpected authAzcopyEnv: %v, Unexpected error: %v", authAzcopyEnv, err)
			}
		})
	})
	ginkgo.When("AADClientSecret be nil and useManagedIdentityExtension is false", func() {
		ginkgo.It("should fail", func(_ context.Context) {

			d.cloud = &azure.Cloud{
				Config: azure.Config{
					AzureClientConfig: config.AzureClientConfig{},
				},
			}
			expectedAuthAzcopyEnv := []string{}
			expected := fmt.Errorf("neither the service principal nor the managed identity has been set")
			authAzcopyEnv, err := d.authorizeAzcopyWithIdentity()
			if !reflect.DeepEqual(authAzcopyEnv, expectedAuthAzcopyEnv) || !reflect.DeepEqual(err, expected) {
				ginkgo.GinkgoT().Errorf("Unexpected authAzcopyEnv: %v, Unexpected error: %v", authAzcopyEnv, err)
			}
		})
	})

})

var _ = ginkgo.Describe("TestGetAzcopyAuth", func() {
	var d *Driver
	ginkgo.BeforeEach(func() {
		d = NewFakeDriver()
	})
	ginkgo.When("failed to get accountKey in secrets", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			d.cloud = &azure.Cloud{
				Config: azure.Config{},
			}
			secrets := map[string]string{
				defaultSecretAccountName: "accountName",
			}

			expectedAccountSASToken := ""
			expectedErr := fmt.Errorf("could not find accountkey or azurestorageaccountkey field in secrets")
			accountSASToken, authAzcopyEnv, err := d.getAzcopyAuth(ctx, "accountName", "", "core.windows.net", &azure.AccountOptions{}, secrets, "secretsName", "secretsNamespace", false)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
			gomega.Expect(authAzcopyEnv).To(gomega.BeNil())
			gomega.Expect(accountSASToken).To(gomega.Equal(expectedAccountSASToken))
		})
	})
	ginkgo.When("generate SAS token failed for illegal account key", func() {
		ginkgo.It("should fail", func(ctx context.Context) {
			d.cloud = &azure.Cloud{
				Config: azure.Config{},
			}
			secrets := map[string]string{
				defaultSecretAccountName: "accountName",
				defaultSecretAccountKey:  "fakeValue",
			}

			expectedAccountSASToken := ""
			expectedErr := status.Errorf(codes.Internal, "failed to generate sas token in creating new shared key credential, accountName: %s, err: %s", "accountName", "decode account key: illegal base64 data at input byte 8")
			accountSASToken, authAzcopyEnv, err := d.getAzcopyAuth(ctx, "accountName", "", "core.windows.net", &azure.AccountOptions{}, secrets, "secretsName", "secretsNamespace", false)
			gomega.Expect(err).To(gomega.Equal(expectedErr))
			gomega.Expect(authAzcopyEnv).To(gomega.BeNil())
			gomega.Expect(accountSASToken).To(gomega.Equal(expectedAccountSASToken))
		})
	})
})
