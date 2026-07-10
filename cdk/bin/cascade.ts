#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { BaselineStack } from '../lib/baseline-stack';

const app = new cdk.App();
new BaselineStack(app, 'CascadeBaseline', {
    env: {
        account: process.env.CDK_DEFAULT_ACCOUNT,
        region: process.env.CDK_DEFAULT_REGION,
    },
});
