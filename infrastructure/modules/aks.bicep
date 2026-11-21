// AKS with the add-ons the workloads rely on: workload identity, the Key Vault secrets
// provider, the application routing ingress controller, managed Prometheus and Container
// Insights.
param location string
param baseName string
param tags object
param isProd bool
param kubernetesVersion string
param nodeSubnetId string
param controlPlaneIdentityId string
param logAnalyticsWorkspaceId string
param monitorWorkspaceId string

resource cluster 'Microsoft.ContainerService/managedClusters@2024-05-01' = {
  name: 'aks-${baseName}'
  location: location
  tags: tags
  sku: {
    name: 'Base'
    // The Standard tier adds an uptime SLA for the API server.
    tier: isProd ? 'Standard' : 'Free'
  }
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${controlPlaneIdentityId}': {}
    }
  }
  properties: {
    kubernetesVersion: kubernetesVersion
    dnsPrefix: baseName
    // Access goes through Entra ID and Azure RBAC only, never shared certificates.
    disableLocalAccounts: true
    aadProfile: {
      managed: true
      enableAzureRBAC: true
    }
    agentPoolProfiles: [
      {
        name: 'system'
        mode: 'System'
        vmSize: 'Standard_D4ds_v5'
        osSKU: 'AzureLinux'
        count: isProd ? 3 : 1
        enableAutoScaling: false
        availabilityZones: isProd ? ['1', '2', '3'] : null
        vnetSubnetID: nodeSubnetId
        // Keeps application pods off the nodes that run cluster add-ons.
        nodeTaints: ['CriticalAddonsOnly=true:NoSchedule']
      }
      {
        name: 'apps'
        mode: 'User'
        vmSize: 'Standard_D4ds_v5'
        osSKU: 'AzureLinux'
        enableAutoScaling: true
        minCount: isProd ? 3 : 1
        maxCount: isProd ? 12 : 3
        availabilityZones: isProd ? ['1', '2', '3'] : null
        vnetSubnetID: nodeSubnetId
      }
    ]
    networkProfile: {
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      // Cilium enforces the NetworkPolicies in apps/base.
      networkDataplane: 'cilium'
      networkPolicy: 'cilium'
      podCidr: '192.168.0.0/16'
      serviceCidr: '10.21.0.0/16'
      dnsServiceIP: '10.21.0.10'
      outboundType: 'loadBalancer'
    }
    oidcIssuerProfile: {
      enabled: true
    }
    securityProfile: {
      workloadIdentity: {
        enabled: true
      }
    }
    autoUpgradeProfile: {
      upgradeChannel: 'patch'
      nodeOSUpgradeChannel: 'NodeImage'
    }
    ingressProfile: {
      webAppRouting: {
        enabled: true
      }
    }
    addonProfiles: {
      azureKeyvaultSecretsProvider: {
        enabled: true
        config: {
          enableSecretRotation: 'true'
          rotationPollInterval: '2m'
        }
      }
      omsagent: {
        enabled: true
        config: {
          logAnalyticsWorkspaceResourceID: logAnalyticsWorkspaceId
          useAADAuth: 'true'
        }
      }
    }
    azureMonitorProfile: {
      metrics: {
        enabled: true
        kubeStateMetrics: {
          metricLabelsAllowlist: ''
          metricAnnotationsAllowList: ''
        }
      }
    }
  }
}

// Managed Prometheus: a collection endpoint and rule send the cluster's metrics, including
// the PodMonitor targets, to the Azure Monitor workspace.
resource prometheusEndpoint 'Microsoft.Insights/dataCollectionEndpoints@2022-06-01' = {
  name: 'dce-${baseName}-prom'
  location: location
  tags: tags
  kind: 'Linux'
  properties: {}
}

resource prometheusRule 'Microsoft.Insights/dataCollectionRules@2022-06-01' = {
  name: 'dcr-${baseName}-prom'
  location: location
  tags: tags
  kind: 'Linux'
  properties: {
    dataCollectionEndpointId: prometheusEndpoint.id
    dataSources: {
      prometheusForwarder: [
        {
          name: 'PrometheusDataSource'
          streams: ['Microsoft-PrometheusMetrics']
          labelIncludeFilter: {}
        }
      ]
    }
    destinations: {
      monitoringAccounts: [
        {
          accountResourceId: monitorWorkspaceId
          name: 'MonitoringAccount1'
        }
      ]
    }
    dataFlows: [
      {
        streams: ['Microsoft-PrometheusMetrics']
        destinations: ['MonitoringAccount1']
      }
    ]
  }
}

resource prometheusAssociation 'Microsoft.Insights/dataCollectionRuleAssociations@2022-06-01' = {
  name: 'MSProm-${location}-${cluster.name}'
  scope: cluster
  properties: {
    dataCollectionRuleId: prometheusRule.id
  }
}

// Container Insights: container logs and inventory into Log Analytics.
resource insightsRule 'Microsoft.Insights/dataCollectionRules@2022-06-01' = {
  name: 'dcr-${baseName}-ci'
  location: location
  tags: tags
  kind: 'Linux'
  properties: {
    dataSources: {
      extensions: [
        {
          name: 'ContainerInsightsExtension'
          streams: ['Microsoft-ContainerInsights-Group-Default']
          extensionName: 'ContainerInsights'
          extensionSettings: {
            dataCollectionSettings: {
              interval: '1m'
              namespaceFilteringMode: 'Off'
              enableContainerLogV2: true
            }
          }
        }
      ]
    }
    destinations: {
      logAnalytics: [
        {
          workspaceResourceId: logAnalyticsWorkspaceId
          name: 'ciworkspace'
        }
      ]
    }
    dataFlows: [
      {
        streams: ['Microsoft-ContainerInsights-Group-Default']
        destinations: ['ciworkspace']
      }
    ]
  }
}

resource insightsAssociation 'Microsoft.Insights/dataCollectionRuleAssociations@2022-06-01' = {
  name: 'ContainerInsightsExtension'
  scope: cluster
  properties: {
    dataCollectionRuleId: insightsRule.id
  }
}

output clusterName string = cluster.name
output clusterId string = cluster.id
