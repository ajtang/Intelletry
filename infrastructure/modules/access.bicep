// Identities and the least access each one needs.
param location string
param baseName string
param tags object
param clusterName string
param registryName string
param keyVaultName string

resource cluster 'Microsoft.ContainerService/managedClusters@2024-05-01' existing = {
  name: clusterName
}

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = {
  name: registryName
}

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' existing = {
  name: keyVaultName
}

var acrPull = '7f951dda-4ed3-4680-a7ca-43fe172d538d'
var keyVaultSecretsUser = '4633458b-17de-408a-b874-0445c86b69e6'

// Nodes pull images with the kubelet identity.
resource kubeletPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, cluster.id, acrPull)
  scope: registry
  properties: {
    principalId: cluster.properties.identityProfile.kubeletidentity.objectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', acrPull)
  }
}

// The ingress controller reads the TLS certificate from the vault.
resource ingressCertificates 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vault.id, cluster.id, 'webapprouting', keyVaultSecretsUser)
  scope: vault
  properties: {
    principalId: cluster.properties.ingressProfile.webAppRouting.identity.objectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', keyVaultSecretsUser)
  }
}

// Workloads read their secrets as this identity, through federated Kubernetes tokens.
resource workloads 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'id-${baseName}-workloads'
  location: location
  tags: tags
}

resource workloadSecrets 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vault.id, workloads.id, keyVaultSecretsUser)
  scope: vault
  properties: {
    principalId: workloads.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', keyVaultSecretsUser)
  }
}

// Only these two Kubernetes service accounts can act as the workload identity.
resource appsFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: workloads
  name: 'intelletry'
  properties: {
    issuer: cluster.properties.oidcIssuerProfile.issuerURL
    subject: 'system:serviceaccount:intelletry:intelletry'
    audiences: ['api://AzureADTokenExchange']
  }
}

// Azure rejects concurrent writes of federated credentials on one identity.
resource collectorFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: workloads
  name: 'otel-collector'
  dependsOn: [appsFederation]
  properties: {
    issuer: cluster.properties.oidcIssuerProfile.issuerURL
    subject: 'system:serviceaccount:observability:otel-collector'
    audiences: ['api://AzureADTokenExchange']
  }
}

output workloadClientId string = workloads.properties.clientId
