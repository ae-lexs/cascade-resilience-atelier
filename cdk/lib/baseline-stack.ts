import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as elbv2 from 'aws-cdk-lib/aws-elasticloadbalancingv2';
import * as rds from 'aws-cdk-lib/aws-rds';

export class BaselineStack extends cdk.Stack {
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
      databaseName: 'cascade',
      credentials: rds.Credentials.fromGeneratedSecret('cascade_app'), // user + generated password in Secrets Manager
      removalPolicy: cdk.RemovalPolicy.DESTROY, // teaching env: destroy cleanly
      backupRetention: cdk.Duration.days(0),
      deleteAutomatedBackups: true,
    });

    // Single edge service.
    const cluster = new ecs.Cluster(this, 'Cluster', { vpc });
    const taskDef = new ecs.FargateTaskDefinition(this, 'TaskDef', { cpu: 256, memoryLimitMiB: 512 });

    const container = taskDef.addContainer('service', {
      image: ecs.ContainerImage.fromAsset('..', { file: 'Dockerfile' }), // builds app/ from repo root
      logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'cascade' }),
      environment: {
        SERVICE_ROLE: 'edge',
        DB_HOST: db.instanceEndpoint.hostname,
        DB_PORT: cdk.Token.asString(db.instanceEndpoint.port),
        DB_NAME: 'cascade',
        DB_USER: 'cascade_app',
      },
      secrets: {
        DB_PASSWORD: ecs.Secret.fromSecretsManager(db.secret!, 'password'),
      },
    });
    container.addPortMappings({ containerPort: 8080 });

    const service = new ecs.FargateService(this, 'Service', {
      cluster,
      taskDefinition: taskDef,
      desiredCount: 2,
      assignPublicIp: true, // pull images via IGW — no NAT (see References)
      vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
      securityGroups: [svcSg],
    });

    // ALB + target group with the NAIVE health check.
    const alb = new elbv2.ApplicationLoadBalancer(this, 'Alb', {
      vpc,
      internetFacing: true,
      securityGroup: albSg,
      vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
    });

    alb.addListener('Http', { port: 80, open: false }).addTargets('Service', {
      port: 8080,
      protocol: elbv2.ApplicationProtocol.HTTP,
      targets: [service],
      healthCheck: {
        path: '/health', // NAIVE — validates nothing. The health-check track changes this line.
        healthyThresholdCount: 2,
        unhealthyThresholdCount: 2,
        interval: cdk.Duration.seconds(30), // defaults; Module 04 tunes to 5s × 6 for a 30s TTE
      },
    });

    new cdk.CfnOutput(this, 'AlbUrl', { value: `http://${alb.loadBalancerDnsName}` });
  }
}
