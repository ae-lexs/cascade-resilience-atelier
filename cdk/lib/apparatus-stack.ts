import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as fis from 'aws-cdk-lib/aws-fis';

interface ApparatusStackProps extends cdk.StackProps {
  dbEndpoint: string;      // for the FIS latency `sources` param
  taskRoleTag: string;     // 'edge' — the FIS target tag value
}

export class ApparatusStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props: ApparatusStackProps) {
    super(scope, id, props);

    // FIS execution role — least privilege for ECS task network faults (ADR-CRA-008).
    const fisRole = new iam.Role(this, 'FisRole', {
      assumedBy: new iam.ServicePrincipal('fis.amazonaws.com'),
    });
    // aws:ecs:task actions deliver the fault via an SSM document to the task's
    // SSM Agent sidecar (see BaselineStack). The FIS role therefore needs SSM
    // command perms in addition to discovering the target tasks — NOT just
    // ecs:DescribeTasks (contra ADR-CRA-008). Verified against the FIS
    // aws:ecs:task-actions requirements, 2026-07-12.
    fisRole.addToPolicy(new iam.PolicyStatement({
      actions: [
        'ecs:DescribeTasks',
        'ecs:ListTasks',
        'ssm:SendCommand',
        'ssm:ListCommands',
        'ssm:CancelCommand',
      ],
      resources: ['*'],
    }));

    // Cascade track: latency ABOVE the 2s DB timeout so each attempt times out and
    // retries, while the query still reaches RDS (so amplification is measurable
    // server-side). Params verified 2026-07-11 via `aws fis get-action`; `sources`
    // accepts a domain name, so the RDS endpoint hostname is valid (and preferred
    // over an IP, which RDS can rotate).
    const dbLatency = new fis.CfnExperimentTemplate(this, 'DbLatency', {
      description: 'DB-path latency into edge tasks (cascade track)',
      roleArn: fisRole.roleArn,
      stopConditions: [{ source: 'none' }],
      tags: { Name: 'cascade-db-latency' },
      targets: {
        edgeTasks: {
          resourceType: 'aws:ecs:task',
          selectionMode: 'ALL',
          resourceTags: { 'cascade:role': props.taskRoleTag },
          // propagateTags:SERVICE stamps cascade:role=edge on EVERY task the
          // service launches, including STOPPED ones from prior deploys, which
          // FIS's tag resolution returns. Those have no SSM managed instance, so
          // without this filter the experiment fails "at least one ECS Task is not
          // registered". Filter to RUNNING (applied to DescribeTasks output;
          // Pascal-case path).
          filters: [{ path: 'LastStatus', values: ['RUNNING'] }],
        },
      },
      actions: {
        latency: {
          actionId: 'aws:ecs:task-network-latency',
          parameters: {
            duration: 'PT5M',
            delayMilliseconds: '3000',              // > 2s DB timeout
            sources: props.dbEndpoint,              // scope to RDS only
            useEcsFaultInjectionEndpoints: 'true',
          },
          targets: { Tasks: 'edgeTasks' },
        },
      },
    });

    // Health-check track: blackhole the DB port so the dependency is fully
    // unreachable while naive /health keeps returning 200 (the fail-silent condition).
    const dbBlackhole = new fis.CfnExperimentTemplate(this, 'DbBlackhole', {
      description: 'DB-port blackhole into edge tasks (health-check track)',
      roleArn: fisRole.roleArn,
      stopConditions: [{ source: 'none' }],
      tags: { Name: 'cascade-db-blackhole' },
      targets: {
        edgeTasks: {
          resourceType: 'aws:ecs:task',
          selectionMode: 'ALL',
          resourceTags: { 'cascade:role': props.taskRoleTag },
          // propagateTags:SERVICE stamps cascade:role=edge on EVERY task the
          // service launches, including STOPPED ones from prior deploys, which
          // FIS's tag resolution returns. Those have no SSM managed instance, so
          // without this filter the experiment fails "at least one ECS Task is not
          // registered". Filter to RUNNING (applied to DescribeTasks output;
          // Pascal-case path).
          filters: [{ path: 'LastStatus', values: ['RUNNING'] }],
        },
      },
      actions: {
        blackhole: {
          actionId: 'aws:ecs:task-network-blackhole-port',
          parameters: {
            duration: 'PT5M',
            port: '5432',
            protocol: 'tcp',
            trafficType: 'egress',
            useEcsFaultInjectionEndpoints: 'true',
          },
          targets: { Tasks: 'edgeTasks' },
        },
      },
    });

    // Surface the experiment-template IDs so the smoke test (§VII) can start them
    // directly: aws fis start-experiment --experiment-template-id <output value>.
    new cdk.CfnOutput(this, 'DbLatencyTemplateId', { value: dbLatency.attrId });
    new cdk.CfnOutput(this, 'DbBlackholeTemplateId', { value: dbBlackhole.attrId });

    // Grafana Cloud CloudWatch data-source role: assumed by Grafana's AWS account
    // with an external ID supplied at apply time via context (never committed —
    // same discipline as feedback_grafana_external_id_var_on_apparatus_apply).
    //   cdk deploy ... -c grafanaAccountId=<id> -c grafanaExternalId=<id>
    const grafanaAccountId = this.node.tryGetContext('grafanaAccountId');
    const grafanaExternalId = this.node.tryGetContext('grafanaExternalId');
    if (grafanaAccountId && grafanaExternalId) {
      new iam.Role(this, 'GrafanaCloudWatchRole', {
        assumedBy: new iam.AccountPrincipal(grafanaAccountId).withConditions({
          StringEquals: { 'sts:ExternalId': grafanaExternalId },
        }),
        description: 'Grafana Cloud CloudWatch data source (metrics + logs read-only)',
        managedPolicies: [
          iam.ManagedPolicy.fromAwsManagedPolicyName('CloudWatchReadOnlyAccess'),
        ],
      });
    } else {
      cdk.Annotations.of(this).addInfo(
        'Grafana CloudWatch role skipped: pass -c grafanaAccountId=<id> -c grafanaExternalId=<id> at apply time to create it.',
      );
    }

    // TODO(log-retention): the /ecs 'cascade' and 'adot' log groups are created by
    // the awsLogs driver in BaselineStack with auto-generated names. Cleanest is to
    // set `logRetention` on those log drivers (in baseline-stack.ts) rather than
    // reference the groups by guessed name across stacks. Left for a follow-up.
  }
}
