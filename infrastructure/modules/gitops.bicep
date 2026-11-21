// Installs Flux as an AKS extension and points it at this repository. From then on the
// cluster's workloads are whatever clusters/<environment> in Git says.
param clusterName string
param gitRepositoryUrl string
param gitBranch string
param clusterPath string
@description('Values Flux substitutes into clusters/<environment>/cluster-settings.yaml.')
param substitutions object

resource cluster 'Microsoft.ContainerService/managedClusters@2024-05-01' existing = {
  name: clusterName
}

resource flux 'Microsoft.KubernetesConfiguration/extensions@2023-05-01' = {
  name: 'flux'
  scope: cluster
  properties: {
    extensionType: 'microsoft.flux'
    autoUpgradeMinorVersion: true
    releaseTrain: 'Stable'
    scope: {
      cluster: {
        releaseNamespace: 'flux-system'
      }
    }
  }
}

resource config 'Microsoft.KubernetesConfiguration/fluxConfigurations@2023-05-01' = {
  name: 'intelletry'
  scope: cluster
  dependsOn: [flux]
  properties: {
    scope: 'cluster'
    namespace: 'flux-system'
    sourceKind: 'GitRepository'
    suspend: false
    gitRepository: {
      url: gitRepositoryUrl
      repositoryRef: {
        branch: gitBranch
      }
      syncIntervalInSeconds: 120
      timeoutInSeconds: 120
    }
    kustomizations: {
      cluster: {
        path: clusterPath
        prune: true
        syncIntervalInSeconds: 300
        timeoutInSeconds: 600
        postBuild: {
          substitute: substitutions
        }
      }
    }
  }
}
