# Portsmith Third-Party Notices

Portsmith is distributed under the GNU Affero General Public License v3.0; see
the repository `LICENSE`. This file lists the third-party components that are
bundled, embedded, or required at build time, and their licenses. It does not
replace the license text shipped with each component.

## Embedded assets

These files are part of the product and are reproduced under their own
licenses.

| Asset | Component | Version | License | Bundled license/notice |
| --- | --- | --- | --- | --- |
| `internal/portsmith/typescript.txt` | TypeScript compiler | 5.9.3 | Apache-2.0 | `internal/portsmith/typescript-LICENSE.txt` |
| `internal/portsmith/typescript-NOTICE.txt` | TypeScript third-party notices | 5.9.3 | See notice | `internal/portsmith/typescript-NOTICE.txt` |
| `internal/portsmith/goja-LICENSE.txt` | goja JavaScript runtime | v0.0.0-20260926152631-39ec2650adc9 | MIT | `internal/portsmith/goja-LICENSE.txt` |
| `internal/portsmith/pith-LICENSE.txt` | Pith coding-agent SDK | v0.0.0-20260930152022-a50c87d65cf1 | AGPL-3.0 | `internal/portsmith/pith-LICENSE.txt` |

TypeScript 5.9.3 is retained as a licensed third-party asset and executed with
goja. It is not a Go reimplementation of the compiler. The TypeScript
`ThirdPartyNotices` includes material from DefinitelyTyped (MIT), Unicode,
DOM/WHATWG (CC BY 4.0 and W3C terms), WebGL (Khronos), and Web Background
Synchronization; the authoritative text is bundled verbatim.

## Direct Go dependencies

| Module | Version | License |
| --- | --- | --- |
| `github.com/dop251/goja` | v0.0.0-20260926152631-39ec2650adc9 | MIT |
| `github.com/minifish-org/pith` | v0.0.0-20260930152022-a50c87d65cf1 | AGPL-3.0 |

## Indirect Go dependencies

These are pulled in transitively by the pinned Pith module (the AWS SDK and
supporting packages are used by Pith's Amazon Bedrock provider). The license
identifiers below are the ones declared by each project; the authoritative
license is the `LICENSE` file shipped in the module under the Go module cache.

| Module | Version | License |
| --- | --- | --- |
| `github.com/aws/aws-sdk-go-v2` | v1.47.1 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream` | v1.7.20 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/config` | v1.33.6 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/credentials` | v1.20.6 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/feature/ec2/imds` | v1.20.1 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/internal/configsources` | v1.5.4 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | v2.8.4 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/internal/v4a` | v1.5.4 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` | v1.63.1 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding` | v1.13.19 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/internal/presigned-url` | v1.14.4 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/signin` | v1.10.1 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/sso` | v1.38.1 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/ssooidc` | v1.43.1 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/sts` | v1.51.1 | Apache-2.0 |
| `github.com/aws/smithy-go` | v1.28.1 | Apache-2.0 |
| `github.com/coder/websocket` | v1.8.15 | MIT |
| `github.com/dlclark/regexp2/v2` | v2.5.2 | MIT |
| `github.com/go-sourcemap/sourcemap` | v2.1.3+incompatible | BSD-3-Clause |
| `github.com/goccy/go-yaml` | v1.19.2 | MIT |
| `github.com/google/pprof` | v0.0.0-20230207041349-798e818bf904 | Apache-2.0 |
| `github.com/sabhiram/go-gitignore` | v0.0.0-20210923224102-525f6e181f06 | MIT |
| `golang.org/x/text` | v0.14.0 | BSD-3-Clause |

The full module graph resolved by `go.sum` also includes standard transitive
helper modules (for example `golang.org/x/sys`, `golang.org/x/tools`,
`golang.org/x/mod`, `golang.org/x/oauth2`, `github.com/Masterminds/semver/v3`,
`github.com/chzyer/readline`, `github.com/dlclark/regexp2`,
`github.com/dop251/goja_nodejs`, `github.com/davecgh/go-spew`,
`github.com/ianlancetaylor/demangle`, `github.com/pmezard/go-difflib`,
`github.com/santhosh-tekuri/jsonschema/v6`, `github.com/sergi/go-diff`,
`github.com/stretchr/testify`, `github.com/stretchr/objx`,
`gopkg.in/check.v1` and `gopkg.in/yaml.v3`). Their license texts are distributed
with the module sources in the Go module cache and are not vendored here.

## Development tools

Go and Git are external development tools required only to compile, verify and
integrate migrated code. They are not bundled with the Portsmith binary and are
not distributed under this notice.

## Pi JavaScript extensions

Pi JavaScript/TypeScript extension files are not bundled or executed. The Go
product provides equivalent extension points through `codingagent.NewToolRegistry`
tool definitions and `codingagent.ToolHooks`. A workspace that relies on a Pi
extension must reimplement the behavior as a Go tool or hook; the adaptation is
reported through progress output.
