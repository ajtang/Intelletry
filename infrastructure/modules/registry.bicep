param location string
param name string
param tags object
@allowed(['Standard', 'Premium'])
param sku string

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' = {
  name: name
  location: location
  tags: tags
  sku: {
    name: sku
  }
  properties: {
    // Pulls use the cluster's managed identity and pushes use CI's federated identity,
    // so no registry password exists.
    adminUserEnabled: false
    anonymousPullEnabled: false
    zoneRedundancy: sku == 'Premium' ? 'Enabled' : 'Disabled'
  }
}

output name string = registry.name
output loginServer string = registry.properties.loginServer
