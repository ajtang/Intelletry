// The cluster's control-plane identity exists before the cluster, so it can be granted
// access to the node subnet without a window where the cluster cannot manage networking.
param location string
param baseName string
param tags object
param nodeSubnetId string

resource controlPlane 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'id-${baseName}-aks'
  location: location
  tags: tags
}

resource vnet 'Microsoft.Network/virtualNetworks@2024-01-01' existing = {
  name: split(nodeSubnetId, '/')[8]
}

resource nodeSubnet 'Microsoft.Network/virtualNetworks/subnets@2024-01-01' existing = {
  parent: vnet
  name: last(split(nodeSubnetId, '/'))
}

var networkContributor = '4d97b98b-1d4f-4787-a291-c67834d212e7'

resource subnetAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(nodeSubnet.id, controlPlane.id, networkContributor)
  scope: nodeSubnet
  properties: {
    principalId: controlPlane.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributor)
  }
}

output controlPlaneIdentityId string = controlPlane.id
