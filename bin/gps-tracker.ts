#!/usr/bin/env node
import 'source-map-support/register';
import { App } from 'aws-cdk-lib';
import { GithubOidcStack } from '../lib/github-oidc-stack';
import { GpsTrackerStack } from '../lib/gps-tracker-stack';

// Fixed so every deploy (local or CI) and scripts/provision-device.sh agree on the region.
const REGION = 'ap-southeast-2';

const app = new App();
const env = { account: process.env.CDK_DEFAULT_ACCOUNT, region: REGION };

new GpsTrackerStack(app, 'GpsTracker', { env });

new GithubOidcStack(app, 'GpsTrackerGithubOidc', {
  env,
  repo: 'Compaurum/gps_tracker_backend',
  environment: 'production',
});
