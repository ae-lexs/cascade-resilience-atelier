#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { BaselineStack } from '../lib/baseline-stack';
import { ApparatusStack } from '../lib/apparatus-stack';

const app = new cdk.App();

const env = {
    account: process.env.CDK_DEFAULT_ACCOUNT,
    region: process.env.CDK_DEFAULT_REGION,
};

const baseline = new BaselineStack(app, 'CascadeBaseline', { env });

new ApparatusStack(app, 'CascadeApparatus', {
    env,
    dbEndpoint: baseline.dbEndpoint,
    taskRoleTag: 'edge',            // blackhole (health-check track) targets the edge
    dbLatencyRoleTag: 'downstream', // latency (cascade track) targets the DB-calling downstream (Module 07)
});
