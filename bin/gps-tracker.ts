#!/usr/bin/env node
import 'source-map-support/register';
import { App } from 'aws-cdk-lib';
import { GpsTrackerStack } from '../lib/gps-tracker-stack';

const app = new App();

new GpsTrackerStack(app, 'GpsTracker', {
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: process.env.CDK_DEFAULT_REGION },
});
