# 1) Setting Up Azure File NFS Storage for Self-Managed HyperShift

## Overview

This procedure configures an Azure File storage account with NFS support for a HyperShift-hosted OpenShift cluster on Azure. The storage account will be accessible from both:

- **Guest cluster worker nodes** — where workloads run
- **Management cluster worker nodes** — where the hosted control plane runs

This cross-VNet access is required because pods using NFS volumes may have their CSI driver components running on management cluster nodes.

---

## Prerequisites

| Requirement | Description |
|-------------|-------------|
| `oc` CLI | Authenticated to both management and guest clusters |
| `az` CLI | Authenticated with permissions to create storage accounts and modify network rules |
| Management cluster | Running self-managed HyperShift with hosted control planes |
| Guest cluster | Already provisioned and accessible |
| Azure role | `Storage Account Contributor` + `Microsoft.Network/virtualNetworks/subnets/joinViaServiceEndpoint/action` |

---

## Phase 1: Gather Network Information

### Step 1.1: Identify Management Cluster Worker Subnet

**Run these commands while connected to the management cluster.**

Connect to the **management cluster** and retrieve infrastructure details:

```bash
# Switch context to management cluster (e.g. oc config use-context <management-cluster-context>)
oc config use-context <management-cluster-context>
```

Get the Azure infrastructure configuration:

```bash
oc get infrastructure cluster -o jsonpath='{.status.platformStatus.azure}' | jq .
```

Extract the network resource group from infrastructure:

```bash
MGMT_NETWORK_RG=$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.azure.networkResourceGroupName}')
echo "Management cluster network RG: $MGMT_NETWORK_RG"
```

```bash
MGMT_VNET=$(az network vnet list --resource-group "$MGMT_NETWORK_RG" --query "[0].name" -o tsv)
MGMT_SUBNET=$(az network vnet subnet list --resource-group "$MGMT_NETWORK_RG" --vnet-name "$MGMT_VNET" --query "[?contains(name,'worker')].name | [0]" -o tsv)
echo "Management cluster VNet: $MGMT_VNET subnet: $MGMT_SUBNET"
```

> **Rationale**: The CSI controller pods may run on management cluster nodes. These pods need network access to mount NFS volumes during provisioning operations.

---

### Step 1.2: Identify Guest Cluster Worker Subnet

HyperShift guest clusters do not have machinesets in `openshift-machine-api`; worker nodes are managed via NodePools on the management cluster. Get guest network details from the **management cluster** using the HostedCluster resource.

**Run these commands while connected to the management cluster.**

```bash
oc get hostedclusters -A
```

Set the HostedCluster name and namespace (replace with your values):

```bash
HOSTED_CLUSTER_NAME="<your-hosted-cluster-name>"
HOSTED_CLUSTER_NAMESPACE="clusters"
```

Get network resource group and subnet ID from the HostedCluster:

```bash
GUEST_NETWORK_RG=$(oc get hostedcluster "$HOSTED_CLUSTER_NAME" -n "$HOSTED_CLUSTER_NAMESPACE" -o jsonpath='{.spec.platform.azure.resourceGroup}')
GUEST_SUBNET_ID=$(oc get hostedcluster "$HOSTED_CLUSTER_NAME" -n "$HOSTED_CLUSTER_NAMESPACE" -o jsonpath='{.spec.platform.azure.subnetID}')
echo "Guest cluster network RG: $GUEST_NETWORK_RG"
echo "Guest cluster subnet ID: $GUEST_SUBNET_ID"
```

Parse VNet and subnet names from the subnet ID:

```bash
GUEST_VNET=$(echo "$GUEST_SUBNET_ID" | sed 's|.*/virtualNetworks/\([^/]*\)/subnets/.*|\1|')
GUEST_SUBNET=$(echo "$GUEST_SUBNET_ID" | sed 's|.*/subnets/||')
echo "Guest cluster VNet: $GUEST_VNET subnet: $GUEST_SUBNET"
```

**Get the cluster resource group for the storage account** (run while connected to the **guest cluster**):

```bash
# Switch context to guest (hosted) cluster
oc config use-context <guest-cluster-context>
```

```bash
GUEST_CLUSTER_RG=$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.azure.resourceGroupName}')
echo "Guest cluster resource group (for storage account): $GUEST_CLUSTER_RG"
```

> **Note**: For HyperShift, `GUEST_CLUSTER_RG` often equals `GUEST_NETWORK_RG`. Use `GUEST_CLUSTER_RG` when creating the storage account so it lives in the same resource group as the guest cluster.

---

### Step 1.3: Verify Subscription and Set Variables

```bash
# Get current subscription
SUBSCRIPTION_ID=$(az account show --query id -o tsv)
echo "Subscription ID: $SUBSCRIPTION_ID"
```

```bash
# Set location (use the same region as guest cluster)
LOCATION=$(az group show --name "$GUEST_CLUSTER_RG" --query location -o tsv)
echo "Location: $LOCATION"
```

```bash
# Define storage account name (must be globally unique, 3-24 lowercase alphanumeric)
STORAGE_ACCOUNT_NAME="nfs$(echo $GUEST_CLUSTER_RG | tr -dc 'a-z0-9' | head -c 18)"
echo "Storage account name: $STORAGE_ACCOUNT_NAME"
```

---

## Phase 2: Enable Service Endpoints on Subnets

Service endpoints must be enabled on both subnets **before** adding network rules to the storage account.

### Step 2.1: Enable Service Endpoint on Management Cluster Subnet

```bash
az network vnet subnet update \
  --resource-group "$MGMT_NETWORK_RG" \
  --vnet-name "$MGMT_VNET" \
  --name "$MGMT_SUBNET" \
  --service-endpoints "Microsoft.Storage"
```

```bash
echo "Service endpoint enabled on management cluster subnet: $MGMT_SUBNET"
```

> **Note**: If the subnet already has service endpoints configured, this command adds `Microsoft.Storage` to the existing list without removing others.

---

### Step 2.2: Enable Service Endpoint on Guest Cluster Subnet

```bash
az network vnet subnet update \
  --resource-group "$GUEST_NETWORK_RG" \
  --vnet-name "$GUEST_VNET" \
  --name "$GUEST_SUBNET" \
  --service-endpoints "Microsoft.Storage"
```

```bash
echo "Service endpoint enabled on guest cluster subnet: $GUEST_SUBNET"
```

---

### Step 2.3: Verify Service Endpoints

Verify management cluster subnet:

```bash
az network vnet subnet show \
  --resource-group "$MGMT_NETWORK_RG" \
  --vnet-name "$MGMT_VNET" \
  --name "$MGMT_SUBNET" \
  --query "serviceEndpoints[?service=='Microsoft.Storage']" -o table
```

Verify guest cluster subnet:

```bash
az network vnet subnet show \
  --resource-group "$GUEST_NETWORK_RG" \
  --vnet-name "$GUEST_VNET" \
  --name "$GUEST_SUBNET" \
  --query "serviceEndpoints[?service=='Microsoft.Storage']" -o table
```

---

## Phase 3: Create Azure Storage Account for NFS

### Step 3.1: Create Premium FileStorage Account

NFS file shares require a **Premium FileStorage** account with **LRS** or **ZRS** replication.

```bash
az storage account create \
  --name "$STORAGE_ACCOUNT_NAME" \
  --resource-group "$GUEST_CLUSTER_RG" \
  --location "$LOCATION" \
  --sku "Premium_LRS" \
  --kind "FileStorage" \
  --https-only false \
  --default-action "Deny" \
  --allow-blob-public-access false \
  --min-tls-version "TLS1_2" \
  --public-network-access "Enabled"
```

```bash
echo "Storage account created: $STORAGE_ACCOUNT_NAME"
```

#### Parameter Explanation

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| `--sku Premium_LRS` | Premium locally-redundant | Required for NFS; provides SSD-backed storage |
| `--kind FileStorage` | FileStorage | Required for Premium file shares and NFS |
| `--https-only false` | Disabled | NFS uses port 2049, not HTTPS |
| `--default-action Deny` | Deny | Block all access except explicitly allowed subnets |
| `--public-network-access Enabled` | Enabled | Required for service endpoint access (traffic still restricted to allowed subnets) |

---

### Step 3.2: Add Network Rules for Both Subnets

Add virtual network rules to allow access from both cluster subnets.

Get fully qualified subnet IDs:

```bash
MGMT_SUBNET_ID=$(az network vnet subnet show \
  --resource-group "$MGMT_NETWORK_RG" \
  --vnet-name "$MGMT_VNET" \
  --name "$MGMT_SUBNET" \
  --query id -o tsv)
```

```bash
GUEST_SUBNET_ID=$(az network vnet subnet show \
  --resource-group "$GUEST_NETWORK_RG" \
  --vnet-name "$GUEST_VNET" \
  --name "$GUEST_SUBNET" \
  --query id -o tsv)
```

Add network rule for management cluster subnet:

```bash
az storage account network-rule add \
  --resource-group "$GUEST_CLUSTER_RG" \
  --account-name "$STORAGE_ACCOUNT_NAME" \
  --subnet "$MGMT_SUBNET_ID"
```

Add network rule for guest cluster subnet:

```bash
az storage account network-rule add \
  --resource-group "$GUEST_CLUSTER_RG" \
  --account-name "$STORAGE_ACCOUNT_NAME" \
  --subnet "$GUEST_SUBNET_ID"
```

---

### Step 3.3: Verify Network Rules

```bash
az storage account network-rule list \
  --resource-group "$GUEST_CLUSTER_RG" \
  --account-name "$STORAGE_ACCOUNT_NAME" \
  --query "virtualNetworkRules[].{Subnet:virtualNetworkResourceId,State:state}" -o table
```

Expected output should show both subnets with state `Succeeded`.

---

## Phase 4: Configure OpenShift Storage Class


### Step 4.1: Create NFS Storage Class on Guest Cluster

Switch to the guest cluster context and create the storage class.

```bash
# Ensure you're connected to the guest cluster
oc config use-context <guest-cluster-context>
```

Create the storage class:

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: azure-file-nfs
provisioner: file.csi.azure.com
parameters:
  protocol: nfs
  skuName: Premium_LRS
  storageAccount: ${STORAGE_ACCOUNT_NAME}
  resourceGroup: ${GUEST_CLUSTER_RG}
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions:
  - nconnect=4
  - noresvport
  - actimeo=30
EOF
```

```bash
echo "Storage class 'azure-file-nfs' created"
```

#### Parameter Explanation

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| `protocol: nfs` | NFS 4.1 | Enables NFS instead of default SMB |
| `skuName: Premium_LRS` | Premium LRS | Must match storage account SKU |
| `storageAccount` | Pre-created account | Uses our specific storage account with network rules |
| `resourceGroup` | Guest cluster RG | Location of the storage account |
| `nconnect=4` | 4 connections | Improves throughput by using multiple TCP connections |
| `noresvport` | Non-reserved ports | Recommended for Azure NFS |
| `actimeo=30` | 30 second cache | Balances consistency and performance |

---

### Step 4.3: Verify Storage Class

```bash
# Verify the storage class was created
oc get storageclass azure-file-nfs -o yaml
```

```bash
# Check that Azure File CSI driver is available
oc get csidrivers file.csi.azure.com
```


===========The steps below are testing only and do not have to be included in official Red Hat documentation===========

## Phase 5: Validation

### Step 5.1: Create Test PVC

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-nfs-pvc
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: azure-file-nfs
  resources:
    requests:
      storage: 100Gi
EOF
```

> **Note**: NFS Premium file shares have a minimum size of 100 GiB.

---

### Step 5.2: Verify PVC is Bound

```bash
# Wait for PVC to bind (may take 1-2 minutes)
oc get pvc test-nfs-pvc -n default -w
```

```bash
# Check the PV was created
oc get pv | grep test-nfs-pvc
```

---

### Step 5.3: Create Test Pod

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: test-nfs-pod
  namespace: default
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  containers:
  - name: test
    image: registry.access.redhat.com/ubi9/ubi:latest
    command: ["sleep", "infinity"]
    volumeMounts:
    - name: nfs-volume
      mountPath: /mnt/nfs
  volumes:
  - name: nfs-volume
    persistentVolumeClaim:
      claimName: test-nfs-pvc
EOF
```

---

### Step 5.4: Verify NFS Mount

```bash
# Wait for pod to be running
oc get pod test-nfs-pod -n default -w
```

```bash
# Verify NFS mount inside the pod
oc exec -n default test-nfs-pod -- df -h /mnt/nfs
```

```bash
oc exec -n default test-nfs-pod -- mount | grep nfs
```

```bash
# Test write operation
oc exec -n default test-nfs-pod -- sh -c \
  "echo 'NFS test successful' > /mnt/nfs/test.txt && cat /mnt/nfs/test.txt"
```

---

### Step 5.5: Cleanup Test Resources

```bash
oc delete pod test-nfs-pod -n default
oc delete pvc test-nfs-pvc -n default
```

---

