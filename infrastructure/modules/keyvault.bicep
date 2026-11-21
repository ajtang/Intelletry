// Holds runtime secrets and the TLS certificate. Pods read secrets through workload
// identity, so no credential for the vault itself is stored anywhere.
param location string
param name string
param tags object
param isProd bool
@secure()
param secrets object

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: name
  location: location
  tags: tags
  properties: {
    tenantId: tenant().tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 90
    // Purge protection cannot be turned off again, so dev keeps it off for easy teardown.
    enablePurgeProtection: isProd ? true : null
    publicNetworkAccess: 'Enabled'
  }
}

resource secret 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = [
  for item in items(secrets): {
    parent: vault
    name: item.key
    properties: {
      value: item.value
    }
  }
]

output name string = vault.name
output uri string = vault.properties.vaultUri
