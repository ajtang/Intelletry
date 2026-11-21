using '../main.bicep'

param environment = 'prod'
param kubernetesVersion = '1.34'
param gitRepositoryUrl = 'https://github.com/ajtang/Intelletry'
param gitBranch = 'main'
param dashboardHost = 'fleet.example.com'
param ingestHost = 'telemetry.example.com'
param tlsCertificateName = 'intelletry-prod-tls'
param alertEmail = readEnvironmentVariable('ALERT_EMAIL')

// Secrets come from the deploying pipeline's environment, never from this file.
param postgresAdminPassword = readEnvironmentVariable('POSTGRES_ADMIN_PASSWORD')
param apiToken = readEnvironmentVariable('API_TOKEN')
param ingestApiKeys = readEnvironmentVariable('INGEST_API_KEYS')
// No simulator in production.
