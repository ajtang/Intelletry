// One virtual network: AKS nodes, the delegated PostgreSQL subnet and private endpoints.
// Data services have no public endpoints.
param location string
param baseName string
param tags object

resource vnet 'Microsoft.Network/virtualNetworks@2024-01-01' = {
  name: 'vnet-${baseName}'
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: ['10.20.0.0/16']
    }
    subnets: [
      {
        name: 'aks-nodes'
        properties: {
          addressPrefix: '10.20.0.0/20'
        }
      }
      {
        name: 'postgres'
        properties: {
          addressPrefix: '10.20.16.0/24'
          delegations: [
            {
              name: 'postgres'
              properties: {
                serviceName: 'Microsoft.DBforPostgreSQL/flexibleServers'
              }
            }
          ]
        }
      }
      {
        name: 'private-endpoints'
        properties: {
          addressPrefix: '10.20.17.0/24'
          privateEndpointNetworkPolicies: 'Disabled'
        }
      }
    ]
  }
}

output vnetId string = vnet.id
output nodeSubnetId string = '${vnet.id}/subnets/aks-nodes'
output postgresSubnetId string = '${vnet.id}/subnets/postgres'
output privateEndpointSubnetId string = '${vnet.id}/subnets/private-endpoints'
