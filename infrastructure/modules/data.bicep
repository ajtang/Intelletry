// PostgreSQL holds diagnostics, alerts and rollups. Redis carries the telemetry streams,
// live fleet state and pub/sub. Both are reachable only from inside the virtual network.
param location string
param baseName string
param tags object
param isProd bool
param vnetId string
param postgresSubnetId string
param privateEndpointSubnetId string
param postgresAdminLogin string
@secure()
param postgresAdminPassword string

resource postgresDns 'Microsoft.Network/privateDnsZones@2020-06-01' = {
  name: '${baseName}.private.postgres.database.azure.com'
  location: 'global'
  tags: tags
}

resource postgresDnsLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2020-06-01' = {
  parent: postgresDns
  name: 'vnet'
  location: 'global'
  properties: {
    registrationEnabled: false
    virtualNetwork: {
      id: vnetId
    }
  }
}

resource postgres 'Microsoft.DBforPostgreSQL/flexibleServers@2024-08-01' = {
  name: 'psql-${baseName}'
  location: location
  tags: tags
  dependsOn: [postgresDnsLink]
  sku: isProd
    ? {
        name: 'Standard_D4ds_v5'
        tier: 'GeneralPurpose'
      }
    : {
        name: 'Standard_B2s'
        tier: 'Burstable'
      }
  properties: {
    version: '16'
    administratorLogin: postgresAdminLogin
    administratorLoginPassword: postgresAdminPassword
    storage: {
      storageSizeGB: isProd ? 256 : 32
      autoGrow: 'Enabled'
    }
    backup: {
      backupRetentionDays: isProd ? 35 : 7
      geoRedundantBackup: isProd ? 'Enabled' : 'Disabled'
    }
    // A standby in another zone takes over automatically if the primary's zone fails.
    highAvailability: {
      mode: isProd ? 'ZoneRedundant' : 'Disabled'
    }
    network: {
      delegatedSubnetResourceId: postgresSubnetId
      privateDnsZoneArmResourceId: postgresDns.id
      publicNetworkAccess: 'Disabled'
    }
    authConfig: {
      activeDirectoryAuth: 'Disabled'
      passwordAuth: 'Enabled'
    }
  }
}

resource database 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2024-08-01' = {
  parent: postgres
  name: 'fleet'
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
}

resource redis 'Microsoft.Cache/redis@2024-03-01' = {
  name: 'redis-${baseName}'
  location: location
  tags: tags
  zones: isProd ? ['1', '2', '3'] : null
  properties: {
    sku: isProd
      ? {
          name: 'Premium'
          family: 'P'
          capacity: 1
        }
      : {
          name: 'Standard'
          family: 'C'
          capacity: 1
        }
    redisVersion: '6'
    enableNonSslPort: false
    minimumTlsVersion: '1.2'
    publicNetworkAccess: 'Disabled'
    redisConfiguration: {
      // Streams hold telemetry that has not been processed yet. Evicting them would lose
      // data silently, so a full cache must refuse writes and push back on ingestion.
      'maxmemory-policy': 'noeviction'
    }
  }
}

resource redisDns 'Microsoft.Network/privateDnsZones@2020-06-01' = {
  name: 'privatelink.redis.cache.windows.net'
  location: 'global'
  tags: tags
}

resource redisDnsLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2020-06-01' = {
  parent: redisDns
  name: 'vnet'
  location: 'global'
  properties: {
    registrationEnabled: false
    virtualNetwork: {
      id: vnetId
    }
  }
}

resource redisEndpoint 'Microsoft.Network/privateEndpoints@2024-01-01' = {
  name: 'pe-redis-${baseName}'
  location: location
  tags: tags
  properties: {
    subnet: {
      id: privateEndpointSubnetId
    }
    privateLinkServiceConnections: [
      {
        name: 'redis'
        properties: {
          privateLinkServiceId: redis.id
          groupIds: ['redisCache']
        }
      }
    ]
  }
}

resource redisEndpointDns 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2024-01-01' = {
  parent: redisEndpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [
      {
        name: 'redis'
        properties: {
          privateDnsZoneId: redisDns.id
        }
      }
    ]
  }
}

output postgresFqdn string = postgres.properties.fullyQualifiedDomainName
output databaseName string = database.name
output redisHostName string = redis.properties.hostName
output redisSslPort int = redis.properties.sslPort
#disable-next-line outputs-should-not-contain-secrets
output redisPrimaryKey string = redis.listKeys().primaryKey
