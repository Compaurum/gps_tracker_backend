import { Aws, CfnOutput, Duration, Stack, StackProps } from 'aws-cdk-lib';
import * as iam from 'aws-cdk-lib/aws-iam';
import { Construct } from 'constructs';

export interface GithubOidcStackProps extends StackProps {
  /** "owner/name" of the GitHub repository allowed to deploy. */
  readonly repo: string;
  /** GitHub environment the deploy job runs in; only that job can assume the role. */
  readonly environment: string;
}

/**
 * Lets GitHub Actions deploy with short-lived credentials (OIDC) instead of stored access keys.
 * Deployed once by hand: CI deploys the app stack but never this one, so it can't widen its own access.
 */
export class GithubOidcStack extends Stack {
  constructor(scope: Construct, id: string, props: GithubOidcStackProps) {
    super(scope, id, props);

    // One per account: if another project already created it, import it with
    // OidcProviderNative.fromOidcProviderArn instead.
    const provider = new iam.OidcProviderNative(this, 'GithubProvider', {
      url: 'https://token.actions.githubusercontent.com',
      clientIds: ['sts.amazonaws.com'],
    });

    const role = new iam.Role(this, 'DeployRole', {
      roleName: 'GpsTrackerGithubDeploy',
      description: `GitHub Actions deploys from ${props.repo} (environment ${props.environment})`,
      maxSessionDuration: Duration.hours(1),
      assumedBy: new iam.WebIdentityPrincipal(provider.oidcProviderArn, {
        StringEquals: {
          'token.actions.githubusercontent.com:aud': 'sts.amazonaws.com',
          'token.actions.githubusercontent.com:sub': `repo:${props.repo}:environment:${props.environment}`,
        },
      }),
    });

    // The role itself can do nothing but hand over to the CDK bootstrap roles, which publish
    // assets and run CloudFormation. That's all `cdk deploy` needs.
    role.addToPolicy(
      new iam.PolicyStatement({
        actions: ['sts:AssumeRole'],
        resources: [`arn:${Aws.PARTITION}:iam::${Aws.ACCOUNT_ID}:role/cdk-hnb659fds-*`],
      }),
    );

    new CfnOutput(this, 'DeployRoleArn', { value: role.roleArn });
  }
}
