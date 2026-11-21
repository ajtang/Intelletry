// Prometheus alert rules for the pipeline, evaluated by Azure Monitor against the
// metrics each service exposes on /metrics. Each rule watches a symptom a person would
// need to act on, not every internal counter.
param location string
param baseName string
param tags object
param clusterName string
param clusterId string
param monitorWorkspaceId string
param alertEmail string
@description('Must match STREAM_SHARDS in apps/base/kustomization.yaml.')
param shardCount int

resource team 'Microsoft.Insights/actionGroups@2023-01-01' = {
  name: 'ag-${baseName}'
  location: 'Global'
  tags: tags
  properties: {
    groupShortName: take(replace(baseName, '-', ''), 12)
    enabled: true
    emailReceivers: [
      {
        name: 'on-call'
        emailAddress: alertEmail
        useCommonAlertSchema: true
      }
    ]
  }
}

var rules = [
  {
    alert: 'TelemetryPublishFailing'
    expression: 'sum(rate(ingest_publish_failures_total[5m])) > 0'
    duration: 'PT5M'
    severity: 1
    description: 'Telemetry ingestion cannot write accepted events to Redis after retries. Vehicles are being told to retry, and data is at risk if this continues.'
  }
  {
    alert: 'TelemetryIngestLatencyHigh'
    expression: 'histogram_quantile(0.99, sum by (le) (rate(ingest_request_duration_seconds_bucket[5m]))) > 0.25'
    duration: 'PT10M'
    severity: 2
    description: 'p99 ingestion latency is above 250 ms. Check Redis latency and ingestion CPU.'
  }
  {
    alert: 'FleetTelemetryDropped'
    expression: 'sum(rate(ingest_events_accepted_total[10m])) < 0.5 * sum(rate(ingest_events_accepted_total[10m] offset 1h))'
    duration: 'PT15M'
    severity: 2
    description: 'Accepted telemetry fell by more than half compared with an hour ago. Vehicles may be unable to reach the ingest endpoint.'
  }
  {
    alert: 'AnalysisLagging'
    expression: 'histogram_quantile(0.95, sum by (le) (rate(engine_event_lag_seconds_bucket[5m]))) > 30'
    duration: 'PT10M'
    severity: 2
    description: 'Telemetry is reaching the anomaly engine more than 30 seconds late at p95, so diagnoses are delayed.'
  }
  {
    alert: 'StreamBacklogGrowing'
    expression: 'sum(max by (shard) (engine_queue_depth)) > 50000'
    duration: 'PT10M'
    severity: 2
    description: 'More than 50,000 telemetry entries are waiting for analysis. The engine may need more replicas, or a shard may be stuck.'
  }
  {
    alert: 'ShardsUnowned'
    expression: 'sum(engine_owned_shards) < ${shardCount}'
    duration: 'PT5M'
    severity: 1
    description: 'Some telemetry shards have no anomaly engine owner, so those vehicles are not being analysed.'
  }
  {
    alert: 'TelemetryDeadLettered'
    expression: 'sum(increase(engine_entries_dead_lettered_total[15m])) > 0'
    duration: 'PT0M'
    severity: 3
    description: 'Undecodable telemetry was moved to the dead-letter stream telemetry:dlq. Inspect it for a client sending malformed data.'
  }
  {
    alert: 'AlertOutboxBacklog'
    expression: 'max(alerts_outbox_depth) > 1000'
    duration: 'PT10M'
    severity: 2
    description: 'Diagnoses are waiting to become alerts. Operators are not seeing new problems.'
  }
  {
    alert: 'OperationsApiErrors'
    expression: 'sum(rate(api_request_duration_seconds_count{code=~"5.."}[5m])) / sum(rate(api_request_duration_seconds_count[5m])) > 0.05'
    duration: 'PT10M'
    severity: 2
    description: 'More than 5% of operations API requests are failing, which breaks the dashboard.'
  }
  {
    alert: 'PodRestartingRepeatedly'
    expression: 'sum by (namespace, pod) (increase(kube_pod_container_status_restarts_total{namespace=~"intelletry|observability"}[15m])) > 3'
    duration: 'PT0M'
    severity: 2
    description: 'A pod restarted more than three times in 15 minutes.'
  }
]

resource pipelineRules 'Microsoft.AlertsManagement/prometheusRuleGroups@2023-03-01' = {
  name: 'prom-${baseName}-pipeline'
  location: location
  tags: tags
  properties: {
    description: 'Telemetry pipeline health'
    scopes: [monitorWorkspaceId, clusterId]
    clusterName: clusterName
    enabled: true
    interval: 'PT1M'
    rules: [
      for rule in rules: {
        alert: rule.alert
        expression: rule.expression
        'for': rule.duration
        severity: rule.severity
        enabled: true
        annotations: {
          description: rule.description
        }
        labels: {
          team: 'connected-systems'
        }
        resolveConfiguration: {
          autoResolved: true
          timeToResolve: 'PT10M'
        }
        actions: [
          {
            actionGroupId: team.id
          }
        ]
      }
    ]
  }
}
