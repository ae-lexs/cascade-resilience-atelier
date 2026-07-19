import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as elbv2 from 'aws-cdk-lib/aws-elasticloadbalancingv2';
import * as rds from 'aws-cdk-lib/aws-rds';
import * as elasticache from 'aws-cdk-lib/aws-elasticache';
import * as secretsmanager from 'aws-cdk-lib/aws-secretsmanager';
import * as iam from 'aws-cdk-lib/aws-iam';
import { Platform } from 'aws-cdk-lib/aws-ecr-assets';
import * as fs from 'fs';

export class BaselineStack extends cdk.Stack {
  public readonly dbEndpoint: string;

  constructor(scope: Construct, id: string, props?: cdk.StackProps) {
    super(scope, id, props);

    // Network: 2 AZs, public subnets for Fargate, isolated subnets for RDS, NO NAT.
    const vpc = new ec2.Vpc(this, 'Vpc', {
      maxAzs: 2,
      natGateways: 0, // teaching-grade cost: Fargate reaches ECR via public IP + IGW
      subnetConfiguration: [
        { name: 'public', subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 },
        { name: 'db', subnetType: ec2.SubnetType.PRIVATE_ISOLATED, cidrMask: 24 },
      ],
    });

    // Strict, one-directional security groups.
    const albSg = new ec2.SecurityGroup(this, 'AlbSg', { vpc, description: 'ALB ingress 80' });
    albSg.addIngressRule(ec2.Peer.anyIpv4(), ec2.Port.tcp(80), 'public HTTP');

    const svcSg = new ec2.SecurityGroup(this, 'SvcSg', { vpc, description: 'Fargate service' });
    svcSg.addIngressRule(albSg, ec2.Port.tcp(8080), 'ALB to service');

    const dbSg = new ec2.SecurityGroup(this, 'DbSg', { vpc, description: 'RDS from service only' });
    dbSg.addIngressRule(svcSg, ec2.Port.tcp(5432), 'service to Postgres');

    // RDS PostgreSQL: single-AZ, smallest class, fixed capacity (ADR-CRA-005).
    const db = new rds.DatabaseInstance(this, 'Db', {
      engine: rds.DatabaseInstanceEngine.postgres({ version: rds.PostgresEngineVersion.VER_16 }),
      instanceType: ec2.InstanceType.of(ec2.InstanceClass.T4G, ec2.InstanceSize.MICRO),
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
      securityGroups: [dbSg],
      multiAz: false,
      allocatedStorage: 20,
      databaseName: 'cascadedb', // 'cascade' is a reserved word for the Postgres engine
      credentials: rds.Credentials.fromGeneratedSecret('cascade_app'), // user + generated password in Secrets Manager
      removalPolicy: cdk.RemovalPolicy.DESTROY, // teaching env: destroy cleanly
      backupRetention: cdk.Duration.days(0),
      deleteAutomatedBackups: true,
    });

    // Publish the DB host for the ApparatusStack (FIS latency `sources` param).
    this.dbEndpoint = db.instanceEndpoint.hostname;

    // Module 05: a single-node Redis as a genuine SOFT dependency (cache-aside in
    // /echo). Lives in the isolated subnets alongside RDS, reachable only from the
    // service SG. The Module 05 fault is a subnet-NACL deny on 6379 (port-scoped,
    // so RDS on 5432 in the same subnets is unaffected).
    const cacheSg = new ec2.SecurityGroup(this, 'CacheSg', { vpc, description: 'Redis from service only' });
    cacheSg.addIngressRule(svcSg, ec2.Port.tcp(6379), 'service to Redis');

    const cacheSubnets = new elasticache.CfnSubnetGroup(this, 'CacheSubnets', {
      description: 'cache subnets',
      subnetIds: vpc.selectSubnets({ subnetType: ec2.SubnetType.PRIVATE_ISOLATED }).subnetIds,
    });
    const cache = new elasticache.CfnCacheCluster(this, 'Cache', {
      engine: 'redis',
      cacheNodeType: 'cache.t4g.micro',
      numCacheNodes: 1,
      vpcSecurityGroupIds: [cacheSg.securityGroupId],
      cacheSubnetGroupName: cacheSubnets.ref,
    });

    // Shared ECS cluster with a private DNS namespace so the edge can discover the
    // downstream service by name (edge → downstream.cascade.local, Module 07).
    const cluster = new ecs.Cluster(this, 'Cluster', { vpc });
    cluster.addDefaultCloudMapNamespace({ name: 'cascade.local' }); // DNS_PRIVATE (default)

    // Edge → downstream on 8080 (Layer 2 of the multi-layer cascade). Both
    // services share svcSg, so this is a self-ingress rule.
    svcSg.addIngressRule(svcSg, ec2.Port.tcp(8080), 'edge to downstream');

    // Fail synth loudly rather than bake an empty endpoint into the task def — an
    // empty exporter endpoint crash-loops the collector at runtime, near-invisible
    // during a long deploy (CloudFormation just hangs on the ECS service).
    const grafanaEndpoint = process.env.GRAFANA_OTLP_ENDPOINT;
    if (!grafanaEndpoint) {
      throw new Error(
        'GRAFANA_OTLP_ENDPOINT must be set (the Grafana Cloud OTLP gateway URL) before deploying; ' +
        'it is baked into the ADOT sidecar task definition.',
      );
    }
    const grafanaAuth = secretsmanager.Secret.fromSecretNameV2(this, 'GrafanaAuth', 'cascade/grafana-otlp-auth');

    // --- AWS FIS aws:ecs:task delivery: SSM Agent sidecar + managed-instance role ---
    // FIS injects task-level faults via an SSM document delivered by an SSM Agent
    // in the task, which registers the task as an SSM Managed Instance.
    // enableFaultInjection + pidMode:task are necessary but NOT sufficient (contra
    // ADR-CRA-008) — this is the delivery channel they omit. Shared across roles so
    // either service can be the FIS target (Module 07 targets the downstream).
    const managedInstanceRole = new iam.Role(this, 'FisSsmManagedInstanceRole', {
      assumedBy: new iam.ServicePrincipal('ssm.amazonaws.com'),
      description: 'Attached to ECS tasks registered as SSM managed instances for FIS',
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName('AmazonSSMManagedInstanceCore'),
      ],
    });
    managedInstanceRole.addToPolicy(new iam.PolicyStatement({
      actions: ['ssm:DeleteActivation', 'ssm:DeregisterManagedInstance'],
      resources: ['*'],
    }));

    const ssmActivation = fs.readFileSync('fis-ssm-activation.sh', 'utf8');
    const collectorConfig = fs.readFileSync('../observability/collector.yaml', 'utf8');
    const readyGatesCache = this.node.tryGetContext('readyGatesCache') === 'true' ? 'true' : 'false';
    // Module 08 mitigation: -c breaker=true arms the edge→downstream circuit
    // breaker (and the app drops the Layer-2 retry loop). Default off = Module 07.
    const edgeBreaker = this.node.tryGetContext('breaker') === 'true' ? 'true' : 'false';

    // One Fargate service per role, identical sidecar set (service + ADOT +
    // SSM agent). The SAME image serves both — the Go binary switches behaviour on
    // SERVICE_ROLE / DOWNSTREAM_URL. Env is parameterised per role.
    const makeService = (
      idPrefix: string,
      role: string,
      streamPrefix: string,
      extraEnv: Record<string, string>,
      cloudMapName?: string,
    ): ecs.FargateService => {
      const taskDef = new ecs.FargateTaskDefinition(this, `${idPrefix}TaskDef`, {
        cpu: 512,             // the ADOT sidecar needs headroom
        memoryLimitMiB: 1024,
        runtimePlatform: {
          cpuArchitecture: ecs.CpuArchitecture.X86_64,
          operatingSystemFamily: ecs.OperatingSystemFamily.LINUX,
        },
        pidMode: ecs.PidMode.TASK,   // required by FIS network actions
        enableFaultInjection: true,  // opens the ECS fault-injection endpoints
      });

      const container = taskDef.addContainer('service', {
        // Pin to linux/amd64 so the image matches Fargate's default X86_64
        // platform regardless of build host (e.g. Apple Silicon arm64).
        image: ecs.ContainerImage.fromAsset('..', { file: 'Dockerfile', platform: Platform.LINUX_AMD64 }),
        logging: ecs.LogDrivers.awsLogs({ streamPrefix }),
        environment: {
          SERVICE_ROLE: role,
          DB_HOST: db.instanceEndpoint.hostname,
          DB_PORT: cdk.Token.asString(db.instanceEndpoint.port),
          DB_NAME: 'cascadedb',
          DB_USER: 'cascade_app',
          CACHE_HOST: `${cache.attrRedisEndpointAddress}:${cache.attrRedisEndpointPort}`,
          READY_GATES_CACHE: readyGatesCache,
          OTEL_EXPORTER_OTLP_ENDPOINT: 'http://localhost:4317', // ADOT sidecar
          ...extraEnv,
        },
        secrets: {
          DB_PASSWORD: ecs.Secret.fromSecretsManager(db.secret!, 'password'),
        },
      });
      container.addPortMappings({ containerPort: 8080 });

      // Corroborating trace path only — never take the workload down with it (§I:
      // logs are the measurement spine, traces a visual). Non-essential so a
      // collector failure can't corrupt the numbers it only decorates.
      const adot = taskDef.addContainer('adot', {
        image: ecs.ContainerImage.fromRegistry('public.ecr.aws/aws-observability/aws-otel-collector:latest'),
        command: ['--config=env:AOT_CONFIG_CONTENT'],
        essential: false,
        logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'adot' }),
        environment: {
          AOT_CONFIG_CONTENT: collectorConfig,
          GRAFANA_OTLP_ENDPOINT: grafanaEndpoint,
        },
        secrets: {
          GRAFANA_AUTH: ecs.Secret.fromSecretsManager(grafanaAuth),
        },
      });
      adot.addPortMappings({ containerPort: 4317 });

      // SSM agent sidecar: registers the task on start, deregisters on SIGTERM.
      // Non-essential so the chaos plumbing can never take the workload down.
      taskDef.addContainer('ssm-agent', {
        image: ecs.ContainerImage.fromRegistry('public.ecr.aws/amazon-ssm-agent/amazon-ssm-agent:latest'),
        essential: false,
        entryPoint: ['/bin/bash', '-c'],
        command: [ssmActivation],
        logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'ssm-agent' }),
        environment: {
          MANAGED_INSTANCE_ROLE_NAME: managedInstanceRole.roleName,
        },
      });

      // Task role: create the SSM activation, tag it, and pass the managed-instance role.
      taskDef.taskRole.addToPrincipalPolicy(new iam.PolicyStatement({
        actions: ['ssm:CreateActivation', 'ssm:AddTagsToResource'],
        resources: ['*'],
      }));
      taskDef.taskRole.addToPrincipalPolicy(new iam.PolicyStatement({
        actions: ['iam:PassRole'],
        resources: [managedInstanceRole.roleArn],
      }));

      const svc = new ecs.FargateService(this, `${idPrefix}Service`, {
        cluster,
        taskDefinition: taskDef,
        desiredCount: 2,
        assignPublicIp: true, // pull images via IGW — no NAT (see References)
        vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
        securityGroups: [svcSg],
        propagateTags: ecs.PropagatedTagSource.SERVICE, // push cascade:role onto tasks for FIS targeting
        // Fail a bad deploy in minutes instead of hanging ~3h if tasks can't
        // start; rollback:false leaves the failed state for inspection.
        circuitBreaker: { rollback: false },
        ...(cloudMapName ? { cloudMapOptions: { name: cloudMapName } } : {}),
      });
      cdk.Tags.of(svc).add('cascade:role', role);
      return svc;
    };

    // Downstream owns the DB call (Layer 1, identical to Module 06's queryNow) and
    // is the FIS latency target. All db_attempt lines originate here → the
    // amplification query runs over the 'cascade-downstream' log group.
    makeService('Downstream', 'downstream', 'cascade-downstream', {}, 'downstream');

    // Edge proxies to the downstream with the Layer-2 retry (echoViaDownstream);
    // DOWNSTREAM_URL wired → the edge never runs the DB path directly.
    const edge = makeService('Edge', 'edge', 'cascade', {
      DOWNSTREAM_URL: 'http://downstream.cascade.local:8080',
      EDGE_BREAKER: edgeBreaker, // Module 08: 'true' arms the breaker + drops Layer-2 retry
    });

    // ALB fronts the EDGE only (the downstream is reached via service discovery).
    const alb = new elbv2.ApplicationLoadBalancer(this, 'Alb', {
      vpc,
      internetFacing: true,
      securityGroup: albSg,
      vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
    });

    alb.addListener('Http', { port: 80, open: false }).addTargets('Service', {
      port: 8080,
      protocol: elbv2.ApplicationProtocol.HTTP,
      targets: [edge],
      healthCheck: {
        // Naive /health (no DB) so the DB-latency fault can't fail the health
        // check and evict the faulted downstream tasks mid-run (retry track).
        path: '/health',
        healthyThresholdCount: 2,
        unhealthyThresholdCount: 2,
        interval: cdk.Duration.seconds(30),
      },
    });

    new cdk.CfnOutput(this, 'AlbUrl', { value: `http://${alb.loadBalancerDnsName}` });
  }
}
