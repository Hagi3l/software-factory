# Multi-language stack profiles

See also [`profiles/README.md`](../profiles/README.md).

## Why profiles (not one mega-config)

The factory assumes hostile agents and grades candidates with **independent checks**
in a clean sandbox. Those checks only mean something for a concrete toolchain
(`go test` vs `pytest` vs `pnpm test`). Profiles keep the **same security kernel**
while swapping image + checks + soul prompts.

## CLI

```bash
software-factory profile list
software-factory profile detect --repo /path/to/app
software-factory profile show node
```

## Run examples

### get-chilld / tourney-hub-ai (Node)

```bash
software-factory bootstrap-repo --repo /Users/ve/Projects/get-chilld
docker build -f deploy/node-toolchain.Dockerfile -t factory/node-toolchain:dev .
# get-chilld: bake deps into the sandbox image (zero-network install)
docker build -f deploy/get-chilld.Dockerfile -t factory/get-chilld:dev /Users/ve/Projects/get-chilld
software-factory login
software-factory run --config profiles/node --env get-chilld \
  --repo /Users/ve/Projects/get-chilld \
  --serve-addr 127.0.0.1:8080 --nats-addr 127.0.0.1:4222
```

tourney-hub-ai (generic node image; monorepo has its own pnpm lock — bake a project image similarly if install fails offline):

```bash
software-factory bootstrap-repo --repo /Users/ve/Projects/tourney-hub-ai
software-factory run --config profiles/node --repo /Users/ve/Projects/tourney-hub-ai ...
```

### f5-automation (Python)

```bash
software-factory profile detect --repo /Users/ve/Projects/f5-automation
docker build -f deploy/python-toolchain.Dockerfile -t factory/python-toolchain:dev .
software-factory run --config profiles/python --repo /Users/ve/Projects/f5-automation \
  --serve-addr 127.0.0.1:8080 --nats-addr 127.0.0.1:4222
```

### software-factory itself (Go)

```bash
software-factory run --config config --repo /path/to/software-factory ...
```

## Validate a profile

```bash
software-factory validate --config profiles/node
software-factory validate --config profiles/python
```
