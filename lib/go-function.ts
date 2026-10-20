import { execFileSync } from 'child_process';
import * as path from 'path';
import { DockerImage } from 'aws-cdk-lib';
import * as lambda from 'aws-cdk-lib/aws-lambda';
import { Construct } from 'constructs';

/** Root of the Go module that holds all Lambda handlers. */
const GO_MODULE_DIR = path.join(__dirname, '..', 'lambda');
const GO_IMAGE = 'golang:1.26';

export interface GoFunctionProps extends Omit<lambda.FunctionProps, 'code' | 'handler' | 'runtime' | 'architecture'> {
  /** Package under lambda/cmd/ to build, e.g. "ingest". */
  readonly cmd: string;
}

/**
 * A Lambda built from lambda/cmd/<cmd> as a static arm64 binary on the provided.al2023 runtime.
 * Builds with the local Go toolchain when available, otherwise inside a golang Docker image.
 */
export class GoFunction extends lambda.Function {
  constructor(scope: Construct, id: string, props: GoFunctionProps) {
    const { cmd, ...rest } = props;
    const env = { CGO_ENABLED: '0', GOOS: 'linux', GOARCH: 'arm64' };
    const buildArgs = (outDir: string) => [
      'build', '-tags', 'lambda.norpc', '-trimpath', '-ldflags', '-s -w',
      '-o', path.posix.join(outDir, 'bootstrap'), `./cmd/${cmd}`,
    ];

    super(scope, id, {
      ...rest,
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: 'bootstrap',
      code: lambda.Code.fromAsset(GO_MODULE_DIR, {
        exclude: ['**/*_test.go'],
        bundling: {
          image: DockerImage.fromRegistry(GO_IMAGE),
          environment: { ...env, GOCACHE: '/tmp/go-cache', GOPATH: '/tmp/go' },
          command: ['go', ...buildArgs('/asset-output')],
          local: {
            tryBundle(outputDir: string) {
              try {
                execFileSync('go', ['version'], { stdio: 'ignore' });
              } catch {
                return false;
              }
              execFileSync('go', buildArgs(outputDir), {
                cwd: GO_MODULE_DIR,
                env: { ...process.env, ...env },
                stdio: 'inherit',
              });
              return true;
            },
          },
        },
      }),
    });
  }
}
