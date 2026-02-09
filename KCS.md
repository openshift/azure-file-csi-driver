# Workaround: Fix Failed PVC Restore from VolumeSnapshot (Azure File NFS on HyperShift)

## Overview

When a PVC that is being restored from a VolumeSnapshot stays **Pending** and fails with **403 AuthorizationFailure**, the Azure storage account is likely only allowing the guest (hosted) cluster subnet. The restore operation (azcopy) runs from the **management cluster**; if that subnet is not in the storage account's network rules, Azure returns 403.

This guide applies the workaround: allow the **management cluster worker subnet** (and enable the Microsoft.Storage service endpoint on it), then remove the failed restore PVC and retry the restore.

**Related bug**: [OCPBUGS-69882](https://issues.redhat.com/browse/OCPBUGS-69882) — PVC restore from VolumeSnapshot fails with 403 AuthorizationFailure in HyperShift when the storage account has `defaultAction: Deny` and only the hosted cluster subnet in `virtualNetworkRules`. The verified workaround is to add the management cluster subnet to the storage account's network rules.

---

## Prerequisites

| Requirement | Description |
|-------------|-------------|
| `oc` CLI | Authenticated to both management and guest clusters |
| `az` CLI | Authenticated with permissions to modify storage account network rules and subnets |
| Guest cluster | Has a PVC, a VolumeSnapshot, and a **failed** restore-from-snapshot PVC |
| Azure role | `Storage Account Contributor` + `Microsoft.Network/virtualNetworks/subnets/joinViaServiceEndpoint/action` |

---

## Phase 1: Identify the Storage Account (Guest Cluster)

**Run all steps in this phase while connected to the guest cluster.**

### Step 1.1: Find the Failed Restore PVC

List PVCs that use a VolumeSnapshot as data source and are Pending or show 403/AuthorizationFailure in events:

```bash
oc get pvc -A
```

Inspect the PVC that is restoring from a snapshot (often Pending):

```bash
oc describe pvc <failed-restore-pvc-name> -n <namespace>
```

Confirm it has `dataSource.kind: VolumeSnapshot` and note any 403 or AuthorizationFailure in Events. Set variables for later:

```bash
FAILED_PVC_NAME="<failed-restore-pvc-name>"
FAILED_PVC_NAMESPACE="<namespace>"
```

### Step 1.2: Get the VolumeSnapshot Name

From the failed PVC spec, the snapshot name is in `dataSource.name`:

```bash
SNAPSHOT_NAME=$(oc get pvc "$FAILED_PVC_NAME" -n "$FAILED_PVC_NAMESPACE" -o jsonpath='{.spec.dataSource.name}')
echo "VolumeSnapshot name: $SNAPSHOT_NAME"
```

### Step 1.3: Get the Source PVC (Original Snapshot Source)

The VolumeSnapshot's source is the original PVC that was snapshotted:

```bash
SOURCE_PVC_NAME=$(oc get volumesnapshot "$SNAPSHOT_NAME" -n "$FAILED_PVC_NAMESPACE" -o jsonpath='{.spec.source.persistentVolumeClaimName}')
echo "Source PVC (original): $SOURCE_PVC_NAME"
```

### Step 1.4: Resolve Storage Account Name and Resource Group

The Azure File CSI driver does not expose storage account or resource group in StorageClass parameters or in PV `volumeAttributes`. They are encoded in the PV's `spec.csi.volumeHandle` as `resourceGroup#storageAccountName#fileShareName#...` (see [GetFileShareInfo](https://github.com/kubernetes-sigs/azurefile-csi-driver/blob/master/pkg/azurefile/azurefile.go)).

```bash
PV_NAME=$(oc get pvc "$SOURCE_PVC_NAME" -n "$FAILED_PVC_NAMESPACE" -o jsonpath='{.spec.volumeName}')
VOLUME_HANDLE=$(oc get pv "$PV_NAME" -o jsonpath='{.spec.csi.volumeHandle}')
STORAGE_ACCOUNT_RG=$(echo "$VOLUME_HANDLE" | cut -d'#' -f1)
STORAGE_ACCOUNT_NAME=$(echo "$VOLUME_HANDLE" | cut -d'#' -f2)
echo "Storage account: $STORAGE_ACCOUNT_NAME in resource group: $STORAGE_ACCOUNT_RG"
```

Ensure `STORAGE_ACCOUNT_NAME` and `STORAGE_ACCOUNT_RG` are set before continuing.

---

## Phase 2: Get Management Cluster Worker Subnet (Management Cluster)

**Switch to the management cluster** and retrieve the worker subnet so it can be added to the storage account.

### Step 2.1: Get Management Network Resource Group

```bash
MGMT_NETWORK_RG=$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.azure.networkResourceGroupName}')
echo "Management cluster network RG: $MGMT_NETWORK_RG"
```

### Step 2.2: Get VNet and Worker Subnet

```bash
MGMT_VNET=$(az network vnet list --resource-group "$MGMT_NETWORK_RG" --query "[0].name" -o tsv)
MGMT_SUBNET=$(az network vnet subnet list --resource-group "$MGMT_NETWORK_RG" --vnet-name "$MGMT_VNET" --query "[?contains(name,'worker')].name | [0]" -o tsv)
echo "Management VNet: $MGMT_VNET subnet: $MGMT_SUBNET"
```

---

## Phase 3: Enable Microsoft.Storage Service Endpoint on Management Subnet (Azure CLI)

**No cluster context required**; these commands use `az` and the variables from Phase 2.

Service endpoints must be enabled on the management subnet **before** adding a network rule to the storage account.

### Step 3.1: Enable Service Endpoint

```bash
az network vnet subnet update \
  --resource-group "$MGMT_NETWORK_RG" \
  --vnet-name "$MGMT_VNET" \
  --name "$MGMT_SUBNET" \
  --service-endpoints "Microsoft.Storage"
```

> **Note**: If the subnet already has service endpoints, this adds `Microsoft.Storage` without removing others.

### Step 3.2: Verify Service Endpoint

```bash
az network vnet subnet show \
  --resource-group "$MGMT_NETWORK_RG" \
  --vnet-name "$MGMT_VNET" \
  --name "$MGMT_SUBNET" \
  --query "serviceEndpoints[?service=='Microsoft.Storage']" -o table
```

---

## Phase 4: Add Management Cluster Subnet to Storage Account (Azure CLI)

Use the storage account identified in Phase 1 and the management subnet from Phase 2.

### Step 4.1: Get Management Subnet ID

```bash
MGMT_SUBNET_ID=$(az network vnet subnet show \
  --resource-group "$MGMT_NETWORK_RG" \
  --vnet-name "$MGMT_VNET" \
  --name "$MGMT_SUBNET" \
  --query id -o tsv)
echo "Management subnet ID: $MGMT_SUBNET_ID"
```

### Step 4.2: Add Network Rule to the Storage Account

```bash
az storage account network-rule add \
  --resource-group "$STORAGE_ACCOUNT_RG" \
  --account-name "$STORAGE_ACCOUNT_NAME" \
  --subnet "$MGMT_SUBNET_ID"
```

### Step 4.3: Verify Network Rules

```bash
az storage account network-rule list \
  --resource-group "$STORAGE_ACCOUNT_RG" \
  --account-name "$STORAGE_ACCOUNT_NAME" \
  --query "virtualNetworkRules[].{Subnet:virtualNetworkResourceId,State:state}" -o table
```

Expected: both the guest cluster subnet and the management cluster subnet with state `Succeeded`.

---

## Phase 5: Check Failed Restore PVC succeded (Guest Cluster)

**Switch back to the guest cluster.** 

```bash
oc get pvc "$FAILED_PVC_NAME" -n "$FAILED_PVC_NAMESPACE" -o jsonpath='{.status.phase}'
Bound
```