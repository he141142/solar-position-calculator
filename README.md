# solar-position-calculator

Lambda that receives `ForecastGenerated`, works out each customer's deficit or surplus, takes energy from our own plants first, and publishes `DeficitDetected` / `SurplusDetected` for whatever is left. It serves every zone; the zone comes from the event.

**This repo only holds the code.** The Lambda "shell" (function name, role, rule, DLQ, alarm) lives in [`solar-infra`](https://github.com/he141142/solar-infra), in the `trading` project's `app` layer (infra defines the resource, the app owns its release cycle).

## Release
| Trigger | Result |
|---|---|
| PR | vet + test + build |
| Push `main` | build → push `:<sha>` and `:dev` → `update-function-code` on dev |
| Run workflow manually, pick `stg` / `prd` | same steps against that environment's account (GitHub environment requires approval) |

The `:<sha>` tag is immutable; `:<env>` points at the image currently running. **Rollback:** re-run the workflow on the old commit.

## One-time GitHub setup
- Environments `dev`, `stg`, `prd`, each with secret `AWS_DEPLOY_ROLE_ARN` = output `PositionCalculatorDeployRoleArn` of the `trading-<env>-foundation` stack.
- If `solar-shared` is private: secret `GO_MODULES_TOKEN` (fine-grained PAT with read access to `solar-shared`).
- `stg`/`prd`: enable required reviewers.

## Local development
```bash
go vet ./... && go test ./...
# build the image locally (same as CI)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags lambda.norpc -o dist/bootstrap .
docker build --platform linux/amd64 -t position-calculator .
```
For local work alongside `solar-shared`, use the `go.work` in the parent folder (not committed).

Environment variables (set by infra): `TABLE_NAME`, `EVENT_BUS_NAME`. Config: Parameter Store `/solar-trading/zones/<zone>/*`.
