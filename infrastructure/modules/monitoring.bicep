// Azure Monitor: Log Analytics for container logs, Application Insights for traces, and an
// Azure Monitor workspace for managed Prometheus metrics.
param location string
param baseName string
param tags object
param retentionDays int

resource logs 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: 'log-${baseName}'
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: retentionDays
  }
}

resource appInsights 'Microsoft.Insights/components@2020-02-02' = {
  name: 'appi-${baseName}'
  location: location
  tags: tags
  kind: 'other'
  properties: {
    Application_Type: 'other'
    WorkspaceResourceId: logs.id
    IngestionMode: 'LogAnalytics'
  }
}

resource prometheus 'Microsoft.Monitor/accounts@2023-04-03' = {
  name: 'amw-${baseName}'
  location: location
  tags: tags
}

output logAnalyticsWorkspaceId string = logs.id
output monitorWorkspaceId string = prometheus.id
#disable-next-line outputs-should-not-contain-secrets
output appInsightsConnectionString string = appInsights.properties.ConnectionString
