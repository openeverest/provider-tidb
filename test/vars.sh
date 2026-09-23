#!/bin/bash

# Environment variables sourced by the `make test-integration*` targets before
# running chainsaw. Use it to pin the operator/engine versions your tests run
# against so they are reproducible locally and in CI.
#
# Reference values in chainsaw test files via ($values) bindings or plain
# environment substitution in `script:` steps.

export PROVIDER_ROOT_PATH=${PROVIDER_ROOT_PATH:-${PWD}}
echo "PROVIDER_ROOT_PATH=${PROVIDER_ROOT_PATH}"

# TiDB Operator v2 version the tests run against.
export TIDB_OPERATOR_VERSION=${TIDB_OPERATOR_VERSION:-"v2.0.1"}
echo "TIDB_OPERATOR_VERSION=${TIDB_OPERATOR_VERSION}"

# TiDB engine version (matches the default version bundle in definition/versions.yaml).
export TIDB_ENGINE_VERSION=${TIDB_ENGINE_VERSION:-"v8.5.2"}
echo "TIDB_ENGINE_VERSION=${TIDB_ENGINE_VERSION}"
