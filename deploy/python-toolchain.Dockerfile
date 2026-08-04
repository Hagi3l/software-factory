# Sandbox profile image for the `python-toolchain` profile (profiles/python).
#
# Build from the repo root:
#   docker build -f deploy/python-toolchain.Dockerfile -t factory/python-toolchain:dev .
#
# Contains: Python 3.12, pip, ruff, mypy, pytest, git, factory-python-check.

FROM python:3.12-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl bash jq \
 && rm -rf /var/lib/apt/lists/* \
 && git config --global user.email "agent@factory.local" \
 && git config --global user.name "factory agent" \
 && git config --global init.defaultBranch main \
 && pip install --no-cache-dir \
      "ruff>=0.4" \
      "mypy>=1.10" \
      "pytest>=8.2" \
      "pytest-cov>=5.0" \
      "pip>=24"

COPY deploy/scripts/factory-python-check /usr/local/bin/factory-python-check
RUN chmod +x /usr/local/bin/factory-python-check

WORKDIR /work
CMD ["sleep", "infinity"]
