using '../main.bicep'

param environment = 'dev'
param kubernetesVersion = '1.34'
param gitRepositoryUrl = 'https://github.com/ajtang/Intelletry'
param gitBranch = 'main'
param dashboardHost = 'fleet-dev.example.com'
param ingestHost = 'telemetry-dev.example.com'
param tlsCertificateName = 'intelletry-dev-tls'
param alertEmail = readEnvironmentVariable('ALERT_EMAIL')

// Secrets come from the deploying pipeline's environment, never from this file.
param postgresAdminPassword = readEnvironmentVariable('POSTGRES_ADMIN_PASSWORD')
param apiToken = readEnvironmentVariable('API_TOKEN')
param ingestApiKeys = readEnvironmentVariable('INGEST_API_KEYS')
param simulatorIngestKey = readEnvironmentVariable('SIMULATOR_INGEST_KEY')
