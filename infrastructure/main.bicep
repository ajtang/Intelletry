// Everything one environment needs, deployed into one resource group:
//   az deployment group create -g <rg> --parameters infrastructure/environments/<env>.bicepparam
// The cluster then pulls its workloads from Git through Flux, so this template never
// deploys application code.
targetScope = 'resourceGroup'

@allowed(['dev', 'prod'])
param environment string
param location string = resourceGroup().location
param namePrefix string = 'intelletry'
param kubernetesVersion string

@description('Git repository Flux syncs from, over HTTPS.')
param gitRepositoryUrl string
param gitBranch string = 'main'

@description('Public host names. DNS records pointing at the ingress IP are managed outside this template.')
param dashboardHost string
param ingestHost string

@description('Name of a certificate already imported into the Key Vault, covering both hosts.')
param tlsCertificateName string

@description('Receives Azure Monitor alert notifications.')
param alertEmail string

param postgresAdminLogin string = 'fleetadmin'
@secure()
param postgresAdminPassword string
@secure()
@description('Bearer token the dashboard uses to call the alert service.')
param apiToken string
@secure()
@description('Comma-separated name:key pairs accepted by telemetry ingestion.')
param ingestApiKeys string
@secure()
@description('The simulator\'s ingest key. Must also appear in ingestApiKeys. Leave empty in production.')
param simulatorIngestKey string = ''

var isProd = environment == 'prod'
var suffix = take(uniqueString(resourceGroup().id), 6)
var baseName = '${namePrefix}-${environment}'
var tags = {
  application: namePrefix
  environment: environment
}

module network 'modules/network.bicep' = {
  name: 'network'
  params: {
    location: location
    baseName: baseName
    tags: tags
  }
}

module monitoring 'modules/monitoring.bicep' = {
  name: 'monitoring'
  params: {
    location: location
    baseName: baseName
    tags: tags
    retentionDays: isProd ? 90 : 30
  }
}

module registry 'modules/registry.bicep' = {
  name: 'registry'
  params: {
    location: location
    name: 'acr${replace(namePrefix, '-', '')}${environment}${suffix}'
    tags: tags
    sku: isProd ? 'Premium' : 'Standard'
  }
}

module data 'modules/data.bicep' = {
  name: 'data'
  params: {
    location: location
    baseName: '${baseName}-${suffix}'
    tags: tags
    isProd: isProd
    vnetId: network.outputs.vnetId
    postgresSubnetId: network.outputs.postgresSubnetId
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    postgresAdminLogin: postgresAdminLogin
    postgresAdminPassword: postgresAdminPassword
  }
}

module identity 'modules/identity.bicep' = {
  name: 'identity'
  params: {
    location: location
    baseName: baseName
    tags: tags
    nodeSubnetId: network.outputs.nodeSubnetId
  }
}

module aks 'modules/aks.bicep' = {
  name: 'aks'
  params: {
    location: location
    baseName: baseName
    tags: tags
    isProd: isProd
    kubernetesVersion: kubernetesVersion
    nodeSubnetId: network.outputs.nodeSubnetId
    controlPlaneIdentityId: identity.outputs.controlPlaneIdentityId
    logAnalyticsWorkspaceId: monitoring.outputs.logAnalyticsWorkspaceId
    monitorWorkspaceId: monitoring.outputs.monitorWorkspaceId
  }
}

module keyVault 'modules/keyvault.bicep' = {
  name: 'keyvault'
  params: {
    location: location
    name: take('kv-${baseName}-${suffix}', 24)
    tags: tags
    isProd: isProd
    secrets: {
      'redis-url': 'rediss://:${data.outputs.redisPrimaryKey}@${data.outputs.redisHostName}:${data.outputs.redisSslPort}/0'
      'database-url': 'postgres://${postgresAdminLogin}:${uriComponent(postgresAdminPassword)}@${data.outputs.postgresFqdn}:5432/fleet?sslmode=require'
      'api-token': apiToken
      'ingest-api-keys': ingestApiKeys
      'simulator-ingest-key': empty(simulatorIngestKey) ? 'unused' : simulatorIngestKey
      'appinsights-connection-string': monitoring.outputs.appInsightsConnectionString
    }
  }
}

module access 'modules/access.bicep' = {
  name: 'access'
  params: {
    location: location
    baseName: baseName
    tags: tags
    clusterName: aks.outputs.clusterName
    registryName: registry.outputs.name
    keyVaultName: keyVault.outputs.name
  }
}

module alerts 'modules/alerts.bicep' = {
  name: 'alerts'
  params: {
    location: location
    baseName: baseName
    tags: tags
    clusterName: aks.outputs.clusterName
    clusterId: aks.outputs.clusterId
    monitorWorkspaceId: monitoring.outputs.monitorWorkspaceId
    alertEmail: alertEmail
    shardCount: 16
  }
}

module gitops 'modules/gitops.bicep' = {
  name: 'gitops'
  dependsOn: [access]
  params: {
    clusterName: aks.outputs.clusterName
    gitRepositoryUrl: gitRepositoryUrl
    gitBranch: gitBranch
    clusterPath: './clusters/${environment}'
    substitutions: {
      ACR_LOGIN_SERVER: registry.outputs.loginServer
      KEYVAULT_NAME: keyVault.outputs.name
      TENANT_ID: tenant().tenantId
      WORKLOAD_CLIENT_ID: access.outputs.workloadClientId
      DASHBOARD_HOST: dashboardHost
      INGEST_HOST: ingestHost
      TLS_CERT_KEYVAULT_URI: '${keyVault.outputs.uri}certificates/${tlsCertificateName}'
    }
  }
}

output clusterName string = aks.outputs.clusterName
output registryLoginServer string = registry.outputs.loginServer
output keyVaultName string = keyVault.outputs.name
output workloadClientId string = access.outputs.workloadClientId
